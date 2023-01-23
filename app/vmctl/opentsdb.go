package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmctl/opentsdb"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmctl/vm"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/cheggaaa/pb/v3"
)

var (
	otsdbAddr        = flag.String("otsdb-addr", "http://localhost:4242", "OpenTSDB server addr")
	otsdbConcurrency = flag.Int("otsdb-concurrency", 1, "Number of concurrently running fetch queries to OpenTSDB per metric")
	/*
		because the defaults are set *extremely* low in OpenTSDB (10-25 results), we will
		set a larger default limit, but still allow a user to increase/decrease it
	*/
	otsdbQueryLimit  = flag.Int("otsdb-query-limit", 100e6, "Result limit on meta queries to OpenTSDB (affects both metric name and tag value queries, recommended to use a value exceeding your largest series)")
	otsdbOffsetDays  = flag.Int64("otsdb-offset-days", 0, "Days to offset our 'starting' point for collecting data from OpenTSDB")
	otsdbHardTSStart = flag.Int64("otsdb-hard-ts-start", 0, "A specific timestamp to start from, will override using an offset")
	otsdbRetentions  = flagutil.NewArrayString("otsdb-retentions", "Retentions patterns to collect on. Each pattern should describe the aggregation performed "+
		"for the query, the row size (in HBase) that will define how long each individual query is, "+
		"and the time range to query for. e.g. sum-1m-avg:1h:3d. "+
		"The first time range defined should be a multiple of the row size in HBase. "+
		"e.g. if the row size is 2 hours, 4h is good, 5h less so. We want each query to land on unique rows.")
	otsdbFilters   = flagutil.NewArrayString("otsdb-filters", "Filters to process for discovering metrics in OpenTSDB")
	otsdbNormalize = flag.Bool("otsdb-normalize", false, "Whether to normalize all data received to lower case before forwarding to VictoriaMetrics")
	otsdbMsecsTime = flag.Bool("otsdb-msecstime", false, "Whether OpenTSDB is writing values in milliseconds or seconds")
)

type otsdbProcessor struct {
	oc      *opentsdb.Client
	im      *vm.Importer
	otsdbcc int
}

type queryObj struct {
	Series    opentsdb.Meta
	Rt        opentsdb.RetentionMeta
	Tr        opentsdb.TimeRange
	StartTime int64
}

func newOtsdbProcessor(oc *opentsdb.Client, im *vm.Importer, otsdbcc int) *otsdbProcessor {
	if otsdbcc < 1 {
		otsdbcc = 1
	}
	return &otsdbProcessor{
		oc:      oc,
		im:      im,
		otsdbcc: otsdbcc,
	}
}

func (op *otsdbProcessor) run(silent, verbose bool) error {
	log.Println("Loading all metrics from OpenTSDB for filters: ", op.oc.Filters)
	var metrics []string
	for _, filter := range op.oc.Filters {
		q := fmt.Sprintf("%s/api/suggest?type=metrics&q=%s&max=%d", op.oc.Addr, filter, op.oc.Limit)
		m, err := op.oc.FindMetrics(q)
		if err != nil {
			return fmt.Errorf("metric discovery failed for %q: %s", q, err)
		}
		metrics = append(metrics, m...)
	}
	if len(metrics) < 1 {
		return fmt.Errorf("found no timeseries to import with filters %q", op.oc.Filters)
	}

	question := fmt.Sprintf("Found %d metrics to import. Continue?", len(metrics))
	if !silent && !prompt(question) {
		return nil
	}
	op.im.ResetStats()
	var startTime int64
	if op.oc.HardTS != 0 {
		startTime = op.oc.HardTS
	} else {
		startTime = time.Now().Unix()
	}
	queryRanges := 0
	// pre-calculate the number of query ranges we'll be processing
	for _, rt := range op.oc.Retentions {
		queryRanges += len(rt.QueryRanges)
	}
	for _, metric := range metrics {
		log.Printf("Starting work on %s", metric)
		serieslist, err := op.oc.FindSeries(metric)
		if err != nil {
			return fmt.Errorf("couldn't retrieve series list for %s : %s", metric, err)
		}
		/*
			Create channels for collecting/processing series and errors
			We'll create them per metric to reduce pressure against OpenTSDB

			Limit the size of seriesCh so we can't get too far ahead of actual processing
		*/
		seriesCh := make(chan queryObj, op.otsdbcc)
		errCh := make(chan error)
		// we're going to make serieslist * queryRanges queries, so we should represent that in the progress bar
		bar := pb.StartNew(len(serieslist) * queryRanges)
		defer func(bar *pb.ProgressBar) {
			bar.Finish()
		}(bar)
		var wg sync.WaitGroup
		wg.Add(op.otsdbcc)
		for i := 0; i < op.otsdbcc; i++ {
			go func() {
				defer wg.Done()
				for s := range seriesCh {
					if err := op.do(s); err != nil {
						errCh <- fmt.Errorf("couldn't retrieve series for %s : %s", metric, err)
						return
					}
					bar.Increment()
				}
			}()
		}
		/*
			Loop through all series for this metric, processing all retentions and time ranges
			requested. This loop is our primary "collect data from OpenTSDB loop" and should
			be async, sending data to VictoriaMetrics over time.

			The idea with having the select at the inner-most loop is to ensure quick
			short-circuiting on error.
		*/
		for _, series := range serieslist {
			for _, rt := range op.oc.Retentions {
				for _, tr := range rt.QueryRanges {
					select {
					case otsdbErr := <-errCh:
						return fmt.Errorf("opentsdb error: %s", otsdbErr)
					case vmErr := <-op.im.Errors():
						return fmt.Errorf("import process failed: %s", wrapErr(vmErr, verbose))
					case seriesCh <- queryObj{
						Tr: tr, StartTime: startTime,
						Series: series, Rt: opentsdb.RetentionMeta{
							FirstOrder: rt.FirstOrder, SecondOrder: rt.SecondOrder, AggTime: rt.AggTime}}:
					}
				}
			}
		}

		// Drain channels per metric
		close(seriesCh)
		wg.Wait()
		close(errCh)
		// check for any lingering errors on the query side
		for otsdbErr := range errCh {
			return fmt.Errorf("Import process failed: \n%s", otsdbErr)
		}
		bar.Finish()
		log.Print(op.im.Stats())
	}
	op.im.Close()
	for vmErr := range op.im.Errors() {
		if vmErr.Err != nil {
			return fmt.Errorf("import process failed: %s", wrapErr(vmErr, verbose))
		}
	}
	log.Println("Import finished!")
	log.Print(op.im.Stats())
	return nil
}

func (op *otsdbProcessor) do(s queryObj) error {
	start := s.StartTime - s.Tr.Start
	end := s.StartTime - s.Tr.End
	data, err := op.oc.GetData(s.Series, s.Rt, start, end, op.oc.MsecsTime)
	if err != nil {
		return fmt.Errorf("failed to collect data for %v in %v:%v :: %v", s.Series, s.Rt, s.Tr, err)
	}
	if len(data.Timestamps) < 1 || len(data.Values) < 1 {
		return nil
	}
	labels := make([]vm.LabelPair, len(data.Tags))
	for k, v := range data.Tags {
		labels = append(labels, vm.LabelPair{Name: k, Value: v})
	}
	ts := vm.TimeSeries{
		Name:       data.Metric,
		LabelPairs: labels,
		Timestamps: data.Timestamps,
		Values:     data.Values,
	}
	if err := op.im.Input(&ts); err != nil {
		return err
	}
	return nil
}

func otsdbImport([]string) {
	fmt.Println("OpenTSDB import mode")

	_, cancel := context.WithCancel(context.Background())
	signalHandler(cancel)

	if *otsdbAddr == "" {
		logger.Fatalf("flag --otsdb-addr is required")
	}
	if len(*otsdbRetentions) == 0 {
		logger.Fatalf("flag --otsdb-retentions should contains at least one retention")
	}

	if err := otsdbFilters.Set("a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, r, s, t, u, v, w, x, y, z"); err != nil {
		logger.Fatalf("error set default values to otsdb-filter flag: %s", err)
	}

	oCfg := opentsdb.Config{
		Addr:       *otsdbAddr,
		Limit:      *otsdbQueryLimit,
		Offset:     *otsdbOffsetDays,
		HardTS:     *otsdbHardTSStart,
		Retentions: *otsdbRetentions,
		Filters:    *otsdbFilters,
		Normalize:  *otsdbNormalize,
		MsecsTime:  *otsdbMsecsTime,
	}
	otsdbClient, err := opentsdb.NewClient(oCfg)
	if err != nil {
		logger.Fatalf("failed to create opentsdb client: %s", err)
	}

	vmCfg := initConfigVM()
	// disable progress bars since openTSDB implementation
	// does not use progress bar pool
	vmCfg.DisableProgressBar = true
	importer, err := vm.NewImporter(vmCfg)
	if err != nil {
		logger.Fatalf("failed to create VM importer: %s", err)
	}
	defer importer.Close()

	otsdbProcessor := newOtsdbProcessor(otsdbClient, importer, *otsdbConcurrency)
	if err := otsdbProcessor.run(*globalSilent, *globalVerbose); err != nil {
		logger.Fatalf("error run otsb processor: %s", err)
	}
}

package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func summarizeMain(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	var baseline fileList
	fs.Var(&baseline, "baseline", "earlier results (JSONL) to compare p50 and p95 against; repeat for several files")
	csvOut := fs.String("csv", "", "also write the statistics to this CSV file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "summarize: no result files")
		return 2
	}
	results, err := readResultFiles(fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "summarize:", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "summarize: the files contain no results")
		return 1
	}
	groups := groupByMode(results)
	writeTable(stdout, groups)
	if *csvOut != "" {
		if err := writeCSV(*csvOut, groups); err != nil {
			fmt.Fprintln(os.Stderr, "summarize:", err)
			return 1
		}
	}
	if len(baseline) > 0 {
		old, err := readResultFiles(baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, "summarize:", err)
			return 1
		}
		writeComparison(stdout, compare(groupByMode(old), groups))
	}
	return 0
}

func writeTable(w io.Writer, groups []group) {
	for _, g := range groups {
		_, _ = fmt.Fprintf(w, "mode %s: %d runs", g.Mode, g.Runs)
		if len(g.Excluded) > 0 {
			_, _ = fmt.Fprintf(w, ", %d excluded: %v", len(g.Excluded), g.Excluded)
		}
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintf(w, "%-30s %3s %8s %8s %8s %8s %8s %8s %7s\n", "metric (ms)", "n", "min", "p50", "p90", "p95", "max", "mean", "stdev")
		for _, m := range metrics {
			s, ok := g.Stats[m.Name]
			if !ok {
				continue
			}
			_, _ = fmt.Fprintf(w, "%-30s %3d %8.0f %8.0f %8.0f %8.0f %8.0f %8.0f %7.0f\n", m.Name, s.N, s.Min, s.P50, s.P90, s.P95, s.Max, s.Mean, s.Stdev)
		}
		_, _ = fmt.Fprintln(w)
	}
}

func writeCSV(path string, groups []group) error {
	f, err := os.Create(path) //nolint:gosec // the path is a command-line argument
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"mode", "metric", "n", "min_ms", "p50_ms", "p90_ms", "p95_ms", "max_ms", "mean_ms", "stdev_ms"})
	ms := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	for _, g := range groups {
		for _, m := range metrics {
			s, ok := g.Stats[m.Name]
			if !ok {
				continue
			}
			_ = w.Write([]string{g.Mode, m.Name, strconv.Itoa(s.N), ms(s.Min), ms(s.P50), ms(s.P90), ms(s.P95), ms(s.Max), ms(s.Mean), ms(s.Stdev)})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeComparison(w io.Writer, cs []compared) {
	_, _ = fmt.Fprintf(w, "comparison with the baseline (flagged above +%.0f%% and +%d ms; only meaningful on comparable hardware, cluster and registry)\n", regressionThreshold*100, regressionFloorMs)
	_, _ = fmt.Fprintf(w, "%-6s %-14s %9s %9s %8s %9s %9s %8s\n", "mode", "metric", "old p50", "new p50", "change", "old p95", "new p95", "change")
	for _, c := range cs {
		flag := ""
		if c.Regressed {
			flag = "  REGRESSION"
		}
		_, _ = fmt.Fprintf(w, "%-6s %-14s %9.0f %9.0f %+7.0f%% %9.0f %9.0f %+7.0f%%%s\n",
			c.Mode, c.Metric, c.OldP50, c.NewP50, c.DeltaP50*100, c.OldP95, c.NewP95, c.DeltaP95*100, flag)
	}
}

// fileList is a flag that can be given more than once.
type fileList []string

func (f *fileList) String() string { return strings.Join(*f, ",") }

func (f *fileList) Set(v string) error {
	*f = append(*f, v)
	return nil
}

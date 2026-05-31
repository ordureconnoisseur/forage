package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

type strategyResult struct {
	Name         string
	ScoredScenes int     // scenes that had non-empty truth and contributed to recall
	Precision    float64 // macro avg over scenes where the strategy predicted ≥1
	Recall       float64 // macro avg over scored scenes
	F1           float64
	Failures     []failureRow
	Elapsed      time.Duration
}

type failureRow struct {
	SceneID   string
	Basename  string
	Truth     []string
	Predicted []string
	FalsePos  []string
	FalseNeg  []string
}

func evaluate(s namedStrategy, scenes []stash.LabeledScene, known map[string]bool, haystackFn func(stash.LabeledScene) string) *strategyResult {
	r := &strategyResult{Name: s.Name}
	var sumP, sumR float64
	var nP, nR int

	for _, sc := range scenes {
		truthSet := map[string]bool{}
		for _, id := range sc.PerformerIDs {
			if known[id] {
				truthSet[id] = true
			}
		}
		if len(truthSet) == 0 {
			continue
		}

		predicted := s.Fn(haystackFn(sc))
		predSet := map[string]bool{}
		for _, id := range predicted {
			if known[id] {
				predSet[id] = true
			}
		}

		tp := 0
		var fp, fn []string
		for id := range predSet {
			if truthSet[id] {
				tp++
			} else {
				fp = append(fp, id)
			}
		}
		for id := range truthSet {
			if !predSet[id] {
				fn = append(fn, id)
			}
		}

		// Recall: defined for every scored scene (truth non-empty).
		sumR += float64(tp) / float64(tp+len(fn))
		nR++

		// Precision: only defined for scenes where the strategy
		// predicted at least one performer. Scenes with empty
		// predictions don't penalise precision (they hurt recall via FN).
		if tp+len(fp) > 0 {
			sumP += float64(tp) / float64(tp+len(fp))
			nP++
		}

		if len(fp) > 0 || len(fn) > 0 {
			r.Failures = append(r.Failures, failureRow{
				SceneID:   sc.ID,
				Basename:  sc.Basename,
				Truth:     sortedKeys(truthSet),
				Predicted: sortedKeys(predSet),
				FalsePos:  sortedStr(fp),
				FalseNeg:  sortedStr(fn),
			})
		}
	}

	r.ScoredScenes = nR
	if nR > 0 {
		r.Recall = sumR / float64(nR)
	}
	if nP > 0 {
		r.Precision = sumP / float64(nP)
	}
	if r.Precision+r.Recall > 0 {
		r.F1 = 2 * r.Precision * r.Recall / (r.Precision + r.Recall)
	}
	return r
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStr(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func writeFailuresCSV(path string, rows []failureRow, cap int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"scene_id", "basename", "truth", "predicted", "false_positives", "false_negatives"}); err != nil {
		return err
	}
	n := len(rows)
	if cap > 0 && cap < n {
		n = cap
	}
	for i := 0; i < n; i++ {
		r := rows[i]
		if err := w.Write([]string{
			r.SceneID,
			r.Basename,
			strings.Join(r.Truth, "|"),
			strings.Join(r.Predicted, "|"),
			strings.Join(r.FalsePos, "|"),
			strings.Join(r.FalseNeg, "|"),
		}); err != nil {
			return err
		}
	}
	return nil
}

func printSummaryTable(w io.Writer, results []*strategyResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "strategy\tscenes\tprecision\trecall\tf1\tfailures\telapsed")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%d\t%.3f\t%.3f\t%.3f\t%d\t%s\n",
			r.Name, r.ScoredScenes, r.Precision, r.Recall, r.F1, len(r.Failures), r.Elapsed.Truncate(time.Millisecond))
	}
	tw.Flush()
}

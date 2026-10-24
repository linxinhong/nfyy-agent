// Offline evidence runner only. Uses the actual production HashEmbedder and Chunk.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"enterprise-agent-platform/packages/knowledge"
	"fmt"
	"math"
	"os"
	"sort"
)

type doc struct{ ID, Workspace, Title, Content string }
type query struct {
	ID, Group, Workspace, Text string
	Relevant                   map[string]int
}
type dataset struct {
	ID        string
	TopK      int `json:"top_k"`
	Documents []doc
	Queries   []query
}
type hit struct {
	ID    string  `json:"id"`
	Seq   int     `json:"seq"`
	Score float64 `json:"score"`
}

func main() {
	b, err := os.ReadFile("docs/knowledge-memory/eval/dataset-v1.json")
	must(err)
	var ds dataset
	must(json.Unmarshal(b, &ds))
	digest := sha256.Sum256(b)
	results := []map[string]any{}
	h := knowledge.HashEmbedder{Dims: 256}
	for _, q := range ds.Queries {
		qv, err := h.Embed(context.Background(), q.Text)
		must(err)
		hits := []hit{}
		for _, d := range ds.Documents {
			if d.Workspace != q.Workspace {
				continue
			}
			for seq, c := range knowledge.Chunk(d.Content) {
				v, err := h.Embed(context.Background(), c)
				must(err)
				dot, nq, nv := 0.0, 0.0, 0.0
				for i := range v {
					dot += qv[i] * v[i]
					nq += qv[i] * qv[i]
					nv += v[i] * v[i]
				}
				score := 0.0
				if nq*nv > 0 {
					score = dot / math.Sqrt(nq*nv)
				}
				if score > 0 {
					hits = append(hits, hit{d.ID, seq, score})
				}
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		if len(hits) > ds.TopK {
			hits = hits[:ds.TopK]
		}
		recall, rr, dcg := 0.0, 0.0, 0.0
		seen := map[string]bool{}
		for i, x := range hits {
			if grade := q.Relevant[x.ID]; grade > 0 && !seen[x.ID] {
				seen[x.ID] = true
				recall++
				if rr == 0 {
					rr = 1 / float64(i+1)
				}
				dcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(i+2))
			}
		}
		grades := []int{}
		for _, g := range q.Relevant {
			grades = append(grades, g)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(grades)))
		idcg := 0.0
		for i, g := range grades {
			if i >= ds.TopK {
				break
			}
			idcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i+2))
		}
		var recallMetric, ndcgMetric any
		if len(q.Relevant) > 0 {
			recallMetric = recall / float64(len(q.Relevant))
			ndcgMetric = dcg / idcg
		}
		results = append(results, map[string]any{"query": q.ID, "group": q.Group, "hits": hits, "recall_at_3": recallMetric, "mrr_at_3": rr, "ndcg_at_3": ndcgMetric, "empty": len(hits) == 0})
	}
	out := map[string]any{"dataset_sha256": hex.EncodeToString(digest[:]), "embedder": "knowledge.HashEmbedder{Dims:256}", "source_revision": "0c8abee (knowledge implementation unchanged from 0658a30)", "evidence_scope": "offline ranking only; no SQL, auth, DLP, audit or provider qualification", "results": results}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(out))
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

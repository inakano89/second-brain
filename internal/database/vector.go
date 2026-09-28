package database

import (
	"container/heap"
	"context"
	"encoding/binary"
	"math"
	"runtime"
	"sort"
	"sync"
)

// vectorIndex keeps normalized embeddings in memory, partitioned by model.
type vectorIndex struct {
	mu   sync.RWMutex
	byMd map[string]map[int64][]float32
}

func newVectorIndex() *vectorIndex { return &vectorIndex{byMd: map[string]map[int64][]float32{}} }

func (v *vectorIndex) put(model string, id int64, vec []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for m, set := range v.byMd {
		if m != model {
			delete(set, id)
		}
	}
	set := v.byMd[model]
	if set == nil {
		set = map[int64][]float32{}
		v.byMd[model] = set
	}
	set[id] = vec
}

func (v *vectorIndex) remove(id int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, set := range v.byMd {
		delete(set, id)
	}
}

// Normalize scales vec to unit length in place.
func Normalize(vec []float32) []float32 {
	var s float64
	for _, x := range vec {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return vec
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// SaveEmbedding persists a node embedding and updates the in-memory index.
func (db *DB) SaveEmbedding(ctx context.Context, nodeID int64, model string, vec []float32) error {
	vec = Normalize(append([]float32(nil), vec...))
	_, err := db.ExecContext(ctx, `INSERT INTO embeddings (node_id, model, dim, vector, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET model=excluded.model, dim=excluded.dim, vector=excluded.vector, updated_at=excluded.updated_at`,
		nodeID, model, len(vec), encodeVec(vec), now())
	if err != nil {
		return err
	}
	db.vec.put(model, nodeID, vec)
	return nil
}

func (db *DB) loadVectors(ctx context.Context) error {
	rows, err := db.QueryContext(ctx, `SELECT node_id, model, vector FROM embeddings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var model string
		var blob []byte
		if err := rows.Scan(&id, &model, &blob); err != nil {
			return err
		}
		db.vec.put(model, id, decodeVec(blob))
	}
	return rows.Err()
}

// NodesWithoutEmbedding returns ids lacking an embedding for model.
func (db *DB) NodesWithoutEmbedding(ctx context.Context, model string, limit int) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.id FROM nodes n LEFT JOIN embeddings e ON e.node_id = n.id
		WHERE e.node_id IS NULL OR e.model <> ? ORDER BY n.id DESC LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// VectorOf returns the stored embedding of a node for model.
func (db *DB) VectorOf(model string, id int64) ([]float32, bool) {
	db.vec.mu.RLock()
	defer db.vec.mu.RUnlock()
	v, ok := db.vec.byMd[model][id]
	return v, ok
}

// VectorHit is a similarity search result.
type VectorHit struct {
	ID    int64
	Score float64
}

type hitHeap []VectorHit

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(VectorHit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// VectorSearch returns the top-k cosine matches, scanning shards in parallel goroutines.
func (db *DB) VectorSearch(model string, query []float32, k int, exclude map[int64]bool) []VectorHit {
	if k <= 0 || len(query) == 0 {
		return nil
	}
	q := Normalize(append([]float32(nil), query...))
	db.vec.mu.RLock()
	set := db.vec.byMd[model]
	ids := make([]int64, 0, len(set))
	vecs := make([][]float32, 0, len(set))
	for id, v := range set {
		if len(v) == len(q) && !exclude[id] {
			ids = append(ids, id)
			vecs = append(vecs, v)
		}
	}
	db.vec.mu.RUnlock()
	if len(ids) == 0 {
		return nil
	}
	workers := runtime.NumCPU()
	if workers > len(ids)/256+1 {
		workers = len(ids)/256 + 1
	}
	chunk := (len(ids) + workers - 1) / workers
	results := make([]hitHeap, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min((w+1)*chunk, len(ids))
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			h := make(hitHeap, 0, k+1)
			for i := lo; i < hi; i++ {
				var dot float32
				v := vecs[i]
				for j := range q {
					dot += q[j] * v[j]
				}
				if len(h) < k {
					heap.Push(&h, VectorHit{ids[i], float64(dot)})
				} else if float64(dot) > h[0].Score {
					h[0] = VectorHit{ids[i], float64(dot)}
					heap.Fix(&h, 0)
				}
			}
			results[w] = h
		}(w, lo, hi)
	}
	wg.Wait()
	var all []VectorHit
	for _, h := range results {
		all = append(all, h...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > k {
		all = all[:k]
	}
	return all
}

// VectorCount returns the number of indexed vectors per model.
func (db *DB) VectorCount() map[string]int {
	db.vec.mu.RLock()
	defer db.vec.mu.RUnlock()
	out := map[string]int{}
	for m, s := range db.vec.byMd {
		out[m] = len(s)
	}
	return out
}

// VectorPair is two nodes whose embeddings are very similar.
type VectorPair struct {
	A, B  int64
	Score float64
}

// NearDuplicates compares each query node with every indexed node of model and returns the
// pairs scoring at least threshold (each pair once). Queries run on a goroutine pool.
func (db *DB) NearDuplicates(model string, queries []int64, threshold float64) []VectorPair {
	db.vec.mu.RLock()
	set := db.vec.byMd[model]
	ids := make([]int64, 0, len(set))
	vecs := make([][]float32, 0, len(set))
	for id, v := range set {
		ids = append(ids, id)
		vecs = append(vecs, v)
	}
	qv := make(map[int64][]float32, len(queries))
	for _, id := range queries {
		if v, ok := set[id]; ok {
			qv[id] = v
		}
	}
	db.vec.mu.RUnlock()
	jobs := make(chan int64)
	results := make(chan VectorPair, 64)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range jobs {
				v := qv[q]
				for i, other := range vecs {
					if ids[i] == q || len(other) != len(v) {
						continue
					}
					var dot float32
					for j := range v {
						dot += v[j] * other[j]
					}
					if float64(dot) >= threshold {
						results <- VectorPair{A: q, B: ids[i], Score: float64(dot)}
					}
				}
			}
		}()
	}
	go func() {
		for q := range qv {
			jobs <- q
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	seen := map[[2]int64]bool{}
	var out []VectorPair
	for p := range results {
		key := [2]int64{min(p.A, p.B), max(p.A, p.B)}
		if seen[key] {
			continue
		}
		seen[key] = true
		p.A, p.B = key[0], key[1]
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].A < out[j].A
	})
	return out
}

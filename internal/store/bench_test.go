package store

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shirou/endap/internal/config"
	"github.com/shirou/endap/internal/rank"
	"github.com/shirou/endap/internal/record"
)

// benchLog builds a log that looks like real history: a few hundred distinct
// commands repeated at different rates, with redirects and && in them.
func benchLog(b *testing.B, n int) []byte {
	b.Helper()
	verbs := []string{
		"git status", "git commit --amend --no-edit", "ls -la", "make test",
		"grep -rn foo bar.txt > out.log 2>&1 && echo done", "cd ../..",
		"docker compose up -d", "go test ./... -run TestSomethingRatherLong",
		"kubectl get pods -n production", "ssh deploy@example.internal",
	}
	r := rand.New(rand.NewPCG(1, 2))
	var buf bytes.Buffer
	now := time.Now().UnixMilli()
	for i := range n {
		rec := record.Record{
			V:  record.Version,
			ID: record.NewID(now),
			TS: now - int64(r.IntN(90*24*60*60*1000)),
			// Roughly 300 distinct commands over the whole log.
			Cmd:  fmt.Sprintf("%s %d", verbs[r.IntN(len(verbs))], i%30),
			Cwd:  fmt.Sprintf("/home/u/src/project%d", i%7),
			Host: "thinkpad",
			Sess: "01ABCDEF",
			Sh:   "zsh",
		}
		rec.SetExit(0)
		rec.SetDur(int64(r.IntN(2000)))
		line, err := record.Encode(&rec)
		if err != nil {
			b.Fatal(err)
		}
		buf.Write(line)
	}
	return buf.Bytes()
}

// BenchmarkList is V5: the number is recorded, not asserted. A threshold here
// would only measure how busy the CI machine is.
func BenchmarkList(b *testing.B) {
	for _, n := range []int{100000, 200000} {
		data := benchLog(b, n)
		cfg := config.Defaults()
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportMetric(float64(len(data))/(1<<20), "MiB/log")
			for b.Loop() {
				r := rank.New(cfg, "/home/u/src/project3", true, time.Now().UnixMilli())
				Walk(data, r.Add)
				if got := len(r.Recent()); got == 0 {
					b.Fatal("no entries")
				}
			}
		})
	}
}

// BenchmarkWalk isolates parsing from ranking.
func BenchmarkWalk(b *testing.B) {
	data := benchLog(b, 100000)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if st := Walk(data, func(*record.Record) {}); st.Records != 100000 {
			b.Fatalf("parsed %d records", st.Records)
		}
	}
}

// Command benchmark measures the Yodelr service at realistic scale
// (default: 1M posts).
//
// Usage:
//
//	go run ./cmd/benchmark                     # 1,000,000 posts
//	go run ./cmd/benchmark -posts 10000000     # 10,000,000 posts
//
// It generates the same synthetic workload as the Python solution's
// benchmark.py — Zipf-distributed topic popularity, 0-3 topics per
// post, ~60-120 character texts, every text a distinct string so memory
// numbers are honest — then reports build throughput, query latencies
// and memory use. The RNG differs from CPython's, so the exact post
// stream differs, but the workload distribution is identical, which is
// what the memory comparison needs.
//
// Two memory numbers are reported: peak RSS over baseline (the same
// metric benchmark.py prints, directly comparable) and live heap after
// a forced GC (what the data actually holds, excluding the collector's
// transient headroom).
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kevin907/yodelr-updated/yodelr"
)

var phrases = []string{
	"just finished my morning run feeling great about",
	"can anyone recommend a good place for",
	"hot take nobody is ready to hear about",
	"day 47 of trying to understand",
	"finally shipped the thing, celebrating with",
	"watching the sunset and thinking about",
	"unpopular opinion but i will die on this hill about",
	"my cat just knocked over everything related to",
	"breaking: local developer discovers",
	"weekend plans ruined by",
}

// peakRSSBytes returns this process's peak RSS; ru_maxrss is bytes on
// macOS, KiB on Linux.
func peakRSSBytes() int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss * 1024
}

// liveHeapBytes returns the heap occupied by reachable data, after a
// forced collection.
func liveHeapBytes() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func fmtMiB(nBytes int64) string {
	return fmt.Sprintf("%s MiB", commaInt(int64(math.Round(
		float64(nBytes)/(1<<20)))))
}

func commaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if s[0] == '-' {
		start = 1
	}
	var b strings.Builder
	b.WriteString(s[:start])
	for i := start; i < len(s); i++ {
		if i > start && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// timed runs fn `repeat` times, prints the best wall time, and returns
// the last result.
func timed(label string, repeat int, fn func() int) int {
	best := math.Inf(1)
	result := 0
	for i := 0; i < repeat; i++ {
		t0 := time.Now()
		result = fn()
		best = math.Min(best, time.Since(t0).Seconds())
	}
	unit, value := "s ", best
	if best < 1 {
		unit, value = "ms", best*1e3
	}
	fmt.Printf("  %-58s %8.2f %s\n", label, value, unit)
	return result
}

func main() {
	posts := flag.Int("posts", 1_000_000, "number of posts")
	users := flag.Int("users", 0, "number of users (default: posts / 10)")
	vocab := flag.Int("vocab", 0,
		"distinct topics (default: posts / 20, at least 1000)")
	seed := flag.Uint64("seed", 42, "RNG seed")
	maxResult := flag.Int("max-result", 3_000_000,
		"skip materializing topic result lists larger than this (the "+
			"returned list of strings itself costs ~16 B + text bytes "+
			"per post on the caller's side)")
	flag.Parse()

	nPosts := *posts
	nUsers := *users
	if nUsers == 0 {
		nUsers = max(1, nPosts/10)
	}
	nVocab := *vocab
	if nVocab == 0 {
		nVocab = max(1000, nPosts/20)
	}
	rng := rand.New(rand.NewPCG(*seed, 0))

	fmt.Printf("posts=%s  users=%s  topic vocab=%s  go=%s\n",
		commaInt(int64(nPosts)), commaInt(int64(nUsers)),
		commaInt(int64(nVocab)), runtime.Version())
	baselineRSS := peakRSSBytes()
	fmt.Printf("baseline RSS: %s\n\n", fmtMiB(baselineRSS))

	// Zipf-ish topic popularity (s=1.1): topic k picked with weight
	// 1/(k+1)^1.1.
	weights := make([]float64, nVocab)
	total := 0.0
	for k := range weights {
		weights[k] = 1.0 / math.Pow(float64(k+1), 1.1)
		total += weights[k]
	}
	cum := make([]float64, nVocab)
	acc := 0.0
	for k, w := range weights {
		acc += w / total
		cum[k] = acc
	}
	topicNames := make([]string, nVocab)
	for k := range topicNames {
		topicNames[k] = "topic" + strconv.Itoa(k)
	}
	userNames := make([]string, nUsers)
	for uid := range userNames {
		userNames[uid] = "user" + strconv.Itoa(uid)
	}

	service := yodelr.NewService()
	baselineHeap := liveHeapBytes()

	t0 := time.Now()
	for _, name := range userNames {
		if err := service.AddUser(name); err != nil {
			panic(err)
		}
	}
	tUsers := time.Since(t0).Seconds()
	fmt.Printf("add_user x %s: %.2f s (%s/s)\n",
		commaInt(int64(nUsers)), tUsers,
		commaInt(int64(float64(nUsers)/tUsers)))

	postsByUser := make([]int, nUsers)
	var text strings.Builder
	t0 = time.Now()
	for i := 0; i < nPosts; i++ {
		uid := rng.IntN(nUsers)
		kTopics := rng.Float64()
		text.Reset()
		text.WriteString(phrases[i%len(phrases)])
		if kTopics > 0.15 { // 85% of posts carry 1-3 topics
			for t := 0; t < 1+int(kTopics*3.3)%3; t++ {
				text.WriteString(" #")
				text.WriteString(
					topicNames[sort.SearchFloat64s(cum, rng.Float64())])
			}
		}
		text.WriteString(" n")
		text.WriteString(strconv.Itoa(i))
		if err := service.AddPost(userNames[uid], text.String(),
			int64(i)+1); err != nil {
			panic(err)
		}
		postsByUser[uid]++
	}
	tBuild := time.Since(t0).Seconds()
	peakAfterBuild := peakRSSBytes()
	liveAfterBuild := liveHeapBytes()
	fmt.Printf("add_post x %s: %.2f s (%s posts/s, includes text "+
		"generation)\n", commaInt(int64(nPosts)), tBuild,
		commaInt(int64(float64(nPosts)/tBuild)))
	fmt.Printf("peak RSS after build: %s  (delta %s, %s B/post)\n",
		fmtMiB(peakAfterBuild), fmtMiB(peakAfterBuild-baselineRSS),
		commaInt((peakAfterBuild-baselineRSS)/int64(nPosts)))
	fmt.Printf("live heap after build+GC: %s  (delta %s, %s B/post)\n\n",
		fmtMiB(liveAfterBuild), fmtMiB(liveAfterBuild-baselineHeap),
		commaInt((liveAfterBuild-baselineHeap)/int64(nPosts)))

	heavyUID := 0
	for uid, n := range postsByUser {
		if n > postsByUser[heavyUID] {
			heavyUID = uid
		}
	}
	heavyUser := userNames[heavyUID]
	fmt.Println("queries (best of 3):")
	got := timed(fmt.Sprintf("get_posts_for_user (heaviest: %d posts)",
		postsByUser[heavyUID]), 3, func() int {
		r, err := service.GetPostsForUser(heavyUser)
		if err != nil {
			panic(err)
		}
		return len(r)
	})
	if got != postsByUser[heavyUID] {
		panic(fmt.Sprintf("heaviest user: got %d posts, want %d",
			got, postsByUser[heavyUID]))
	}
	for _, tc := range []struct{ label, topic string }{
		{"hottest topic", "topic0"},
		{"mid topic (#100)", "topic100"},
	} {
		count := service.TopicPostCount(tc.topic)
		if count == 0 {
			continue // tiny runs may never sample this topic
		}
		if count > *maxResult {
			fmt.Printf("  get_posts_for_topic (%s): skipped materializing "+
				"%s posts (> -max-result)\n", tc.label,
				commaInt(int64(count)))
			continue
		}
		n := timed(fmt.Sprintf("get_posts_for_topic (%s)", tc.label), 3,
			func() int { return len(service.GetPostsForTopic(tc.topic)) })
		fmt.Printf("    -> %s posts returned\n", commaInt(int64(n)))
	}
	timed("get_posts_for_topic (rare topic)", 3, func() int {
		return len(service.GetPostsForTopic(topicNames[nVocab-1]))
	})
	timed("get_posts_for_topic (nonexistent topic)", 3, func() int {
		return len(service.GetPostsForTopic("nope"))
	})

	spans := []struct {
		lo, hi int64
		label  string
	}{
		{int64(nPosts - nPosts/100 + 1), int64(nPosts), "last 1% of history"},
		{int64(nPosts - nPosts/10 + 1), int64(nPosts), "last 10% of history"},
		{1, int64(nPosts), "full history"},
	}
	for _, span := range spans {
		n := timed(fmt.Sprintf("get_trending_topics (%s)", span.label), 3,
			func() int {
				return len(service.GetTrendingTopics(span.lo, span.hi))
			})
		fmt.Printf("    -> %s unique topics in window\n", commaInt(int64(n)))
	}

	fmt.Println("\nmutation:")
	t0 = time.Now()
	if err := service.DeleteUser(heavyUser); err != nil {
		panic(err)
	}
	dt := time.Since(t0).Seconds()
	fmt.Printf("  %-58s %8.2f ms\n",
		fmt.Sprintf("delete_user (heaviest user, %d posts)",
			postsByUser[heavyUID]), dt*1e3)
	timed("get_trending_topics (full history, after delete)", 3,
		func() int {
			return len(service.GetTrendingTopics(1, int64(nPosts)))
		})

	fmt.Printf("\nfinal peak RSS: %s\n", fmtMiB(peakRSSBytes()))
	fmt.Printf("final live heap: %s\n", fmtMiB(liveHeapBytes()))
	// Keep the dataset reachable through the final measurements, or the
	// forced GC above collects the entire service before being counted.
	runtime.KeepAlive(service)
}

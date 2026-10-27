// Tests for the Service implementation.
//
// Structure:
//   - spec-level tests, including the exact example from the README;
//   - behavioural tests per API method (ordering, deletion, edge cases);
//   - topic-parsing tests driven through the public API only;
//   - a seeded randomized test that cross-checks the service against a
//     deliberately naive reference model (which parses topics with a
//     regexp, independently of the service's scanner).
package yodelr

import (
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

func mustAddUser(t *testing.T, s *Service, name string) {
	t.Helper()
	if err := s.AddUser(name); err != nil {
		t.Fatalf("AddUser(%q): %v", name, err)
	}
}

func mustAddPost(t *testing.T, s *Service, user, text string, ts int64) {
	t.Helper()
	if err := s.AddPost(user, text, ts); err != nil {
		t.Fatalf("AddPost(%q, %q, %d): %v", user, text, ts, err)
	}
}

// addPosts adds posts with consecutive timestamps start, start+1, ...
func addPosts(t *testing.T, s *Service, user string, start int64, texts ...string) {
	t.Helper()
	for i, text := range texts {
		mustAddPost(t, s, user, text, start+int64(i))
	}
}

func mustPostsForUser(t *testing.T, s *Service, user string) []string {
	t.Helper()
	posts, err := s.GetPostsForUser(user)
	if err != nil {
		t.Fatalf("GetPostsForUser(%q): %v", user, err)
	}
	return posts
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got %q\nwant %q", label, got, want)
	}
}

func assertError(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error, got nil", label)
	}
}

// trendingAll returns trending over a window covering every valid test
// timestamp.
func trendingAll(s *Service) []string {
	return s.GetTrendingTopics(-1e18, 1e18)
}

// ----------------------------------------------------------------------

func TestReadmeExample(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "john")
	mustAddPost(t, s, "john", "just #chilling today", 1)
	mustAddPost(t, s, "john", "eating #steak for dinner", 2)
	mustAddPost(t, s, "john", "ugh! this #steak tasted like dog food", 3)

	assertStrings(t, "posts for john", mustPostsForUser(t, s, "john"),
		[]string{
			"ugh! this #steak tasted like dog food",
			"eating #steak for dinner",
			"just #chilling today",
		})
	assertStrings(t, "posts for steak", s.GetPostsForTopic("steak"),
		[]string{
			"ugh! this #steak tasted like dog food",
			"eating #steak for dinner",
		})
	assertStrings(t, "trending 1-3", s.GetTrendingTopics(1, 3),
		[]string{"steak", "chilling"})
	assertStrings(t, "trending 2-3", s.GetTrendingTopics(2, 3),
		[]string{"steak"})
}

// ----------------------------------------------------------------------
// user management

func TestDuplicateUserFailsAndKeepsExistingPosts(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "first #post", 1)
	assertError(t, "duplicate AddUser", s.AddUser("ada"))
	assertStrings(t, "posts survive the failed re-registration",
		mustPostsForUser(t, s, "ada"), []string{"first #post"})
}

func TestOperationsOnUnknownUserFail(t *testing.T) {
	s := NewService()
	assertError(t, "AddPost", s.AddPost("ghost", "hello", 1))
	assertError(t, "DeleteUser", s.DeleteUser("ghost"))
	_, err := s.GetPostsForUser("ghost")
	assertError(t, "GetPostsForUser", err)
}

func TestUserNamesAreCaseSensitive(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "Ada")
	_, err := s.GetPostsForUser("ada")
	assertError(t, "lowercase lookup of 'Ada'", err)
}

// ----------------------------------------------------------------------
// AddPost

func TestPostOfExactly140CharactersIsAccepted(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	text := strings.Repeat("x", 139) + "!"
	mustAddPost(t, s, "ada", text, 1)
	assertStrings(t, "posts", mustPostsForUser(t, s, "ada"), []string{text})
}

func TestPostLongerThan140CharactersFailsAndStoresNothing(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	assertError(t, "141-char post",
		s.AddPost("ada", "#long "+strings.Repeat("x", 141), 1))
	assertStrings(t, "no post stored", mustPostsForUser(t, s, "ada"), nil)
	assertStrings(t, "no topic stored", s.GetPostsForTopic("long"), nil)
	// The failed call must not have consumed the timestamp.
	mustAddPost(t, s, "ada", "ok", 1)
}

func TestLengthLimitCountsCodePointsNotBytes(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	// 140 astral code points = 560 UTF-8 bytes: the longest legal post.
	text := strings.Repeat("🦀", 140)
	mustAddPost(t, s, "ada", text, 1)
	assertError(t, "141 code points", s.AddPost("ada", text+"x", 2))
	assertStrings(t, "posts", mustPostsForUser(t, s, "ada"), []string{text})
}

func TestEmptyPostAndPostWithoutTopics(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1, "", "no tags in here")
	assertStrings(t, "posts", mustPostsForUser(t, s, "ada"),
		[]string{"no tags in here", ""})
	assertStrings(t, "trending", s.GetTrendingTopics(1, 2), nil)
}

func TestNonIncreasingTimestampsFail(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddUser(t, s, "bob")
	mustAddPost(t, s, "ada", "one", 5)
	assertError(t, "same timestamp", s.AddPost("ada", "same time", 5))
	assertError(t, "earlier timestamp, other user",
		s.AddPost("bob", "earlier, other user", 4))
	mustAddPost(t, s, "bob", "later is fine", 6)
}

func TestSigned64BitTimestampBoundaries(t *testing.T) {
	// Timestamps are int64 by signature, so unlike the Python original
	// there is no out-of-range case to reject — but the exact type
	// boundaries themselves must be storable and queryable.
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "earliest possible", math.MinInt64)
	mustAddPost(t, s, "ada", "latest possible", math.MaxInt64)
	assertStrings(t, "posts", mustPostsForUser(t, s, "ada"),
		[]string{"latest possible", "earliest possible"})
	assertStrings(t, "trending over the full int64 range",
		s.GetTrendingTopics(math.MinInt64, math.MaxInt64), nil)
	assertError(t, "nothing fits after MaxInt64",
		s.AddPost("ada", "beyond the end", math.MaxInt64))
}

// ----------------------------------------------------------------------
// topic parsing (only observable through the API, so assert through it)

func TestSpecParsingExample(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "#programming is #awesome!!", 1)
	assertStrings(t, "trending", trendingAll(s),
		[]string{"awesome", "programming"})
}

func TestPunctuationAndAdjacentTopics(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1,
		"#a#b are adjacent",
		"double ##c hash",
		"trailing #d",
		"#_ and #under_score and #123 and #mix3d_Case",
	)
	assertStrings(t, "trending", trendingAll(s),
		[]string{"123", "_", "a", "b", "c", "d", "mix3d_Case", "under_score"})
}

func TestLoneHashIsNotATopic(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1, "just a # sign", "ends with #")
	assertStrings(t, "trending", trendingAll(s), nil)
}

func TestTopicCharsetIsASCIIOnly(t *testing.T) {
	// Spec: a topic is '#' followed by characters in [0-9a-zA-Z_], so
	// the topic in "#café" ends before the non-ASCII character.
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "drinking coffee at the #café", 1)
	assertStrings(t, "trending", trendingAll(s), []string{"caf"})
	assertStrings(t, "posts for caf", s.GetPostsForTopic("caf"),
		[]string{"drinking coffee at the #café"})
	assertStrings(t, "posts for café", s.GetPostsForTopic("café"), nil)
}

func TestTopicsAreCaseSensitive(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1, "love my #Steak", "another #steak")
	assertStrings(t, "Steak", s.GetPostsForTopic("Steak"),
		[]string{"love my #Steak"})
	assertStrings(t, "steak", s.GetPostsForTopic("steak"),
		[]string{"another #steak"})
}

func TestTopicRepeatedWithinOnePostCountsOnce(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1, "#dup and #dup again", "#dup once more")
	// The first post appears once, not twice.
	assertStrings(t, "posts for dup", s.GetPostsForTopic("dup"),
		[]string{"#dup once more", "#dup and #dup again"})
	// Mention count is per post: 2, not 3 — so "dup" (2) beats "x" (1).
	mustAddPost(t, s, "ada", "an #x post", 3)
	assertStrings(t, "trending", trendingAll(s), []string{"dup", "x"})
}

// ----------------------------------------------------------------------
// GetPosts*

func TestPostsForTopicSpanUsersNewestFirst(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddUser(t, s, "bob")
	mustAddPost(t, s, "ada", "#go ada 1", 1)
	mustAddPost(t, s, "bob", "#go bob 2", 2)
	mustAddPost(t, s, "ada", "#go ada 3", 3)
	assertStrings(t, "posts for go", s.GetPostsForTopic("go"),
		[]string{"#go ada 3", "#go bob 2", "#go ada 1"})
}

func TestUnknownTopicReturnsEmptyList(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "#something", 1)
	assertStrings(t, "unknown topic", s.GetPostsForTopic("nothing"), nil)
}

func TestUserWithNoPosts(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	assertStrings(t, "no posts", mustPostsForUser(t, s, "ada"), nil)
}

// ----------------------------------------------------------------------
// DeleteUser

func TestDeleteRemovesUserAndPostsEverywhere(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddUser(t, s, "bob")
	mustAddPost(t, s, "ada", "ada loves #python and #math", 1)
	mustAddPost(t, s, "bob", "bob loves #python", 2)
	mustAddPost(t, s, "ada", "ada also likes #math", 3)

	if err := s.DeleteUser("ada"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	_, err := s.GetPostsForUser("ada")
	assertError(t, "deleted user lookup", err)
	assertStrings(t, "python survives via bob", s.GetPostsForTopic("python"),
		[]string{"bob loves #python"})
	assertStrings(t, "math is gone", s.GetPostsForTopic("math"), nil)
	assertStrings(t, "trending", s.GetTrendingTopics(1, 3),
		[]string{"python"})
	assertStrings(t, "bob untouched", mustPostsForUser(t, s, "bob"),
		[]string{"bob loves #python"})
}

func TestDeletedNameCanRegisterAgainAsFreshUser(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "old life #retro", 1)
	if err := s.DeleteUser("ada"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	mustAddUser(t, s, "ada")
	assertStrings(t, "fresh user has no posts",
		mustPostsForUser(t, s, "ada"), nil)
	mustAddPost(t, s, "ada", "new life", 2)
	assertStrings(t, "new post only", mustPostsForUser(t, s, "ada"),
		[]string{"new life"})
	assertStrings(t, "old topic is gone", s.GetPostsForTopic("retro"), nil)
}

func TestDeleteUserWithoutPosts(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	if err := s.DeleteUser("ada"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	assertError(t, "second delete", s.DeleteUser("ada"))
}

// ----------------------------------------------------------------------
// stats extensions

func TestCountsTrackAddsDeletesAndReregistration(t *testing.T) {
	s := NewService()
	check := func(label string, users, posts, topics int) {
		t.Helper()
		got := [3]int{s.UserCount(), s.PostCount(), s.TopicCount()}
		if got != [3]int{users, posts, topics} {
			t.Fatalf("%s: (users, posts, topics) = %v, want (%d, %d, %d)",
				label, got, users, posts, topics)
		}
	}
	check("empty service", 0, 0, 0)

	mustAddUser(t, s, "ada")
	mustAddUser(t, s, "bob")
	addPosts(t, s, "ada", 1, "#shared and #ada_only", "#shared again")
	mustAddPost(t, s, "bob", "#shared from bob", 3)
	check("after three posts", 2, 3, 2) // topics: shared, ada_only

	// A failed call must not move any counter.
	assertError(t, "unknown user", s.AddPost("ghost", "#nope", 4))
	check("after failed post", 2, 3, 2)

	if err := s.DeleteUser("ada"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	// ada_only lost its last live post; shared survives via bob.
	check("after delete", 1, 1, 1)

	mustAddUser(t, s, "ada") // re-registration counts as a fresh user
	check("after re-registration", 2, 1, 1)
}

// ----------------------------------------------------------------------
// trending

func TestIntervalBoundsAreInclusive(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 10, "#a", "#b", "#c") // ts 10, 11, 12
	assertStrings(t, "10-12", s.GetTrendingTopics(10, 12),
		[]string{"a", "b", "c"})
	assertStrings(t, "11-11", s.GetTrendingTopics(11, 11), []string{"b"})
	assertStrings(t, "12-12", s.GetTrendingTopics(12, 12), []string{"c"})
}

func TestCountDescThenAlphabetical(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1,
		"#banana #apple #cherry",
		"#banana #apple",
		"#banana",
		"#cherry",
	)
	// banana: 3, apple: 2, cherry: 2 -> apple before cherry on the tie.
	assertStrings(t, "trending", s.GetTrendingTopics(1, 4),
		[]string{"banana", "apple", "cherry"})
}

func TestEmptyAndInvertedIntervals(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	mustAddPost(t, s, "ada", "#a", 10)
	assertStrings(t, "inverted", s.GetTrendingTopics(11, 10), nil)
	assertStrings(t, "before", s.GetTrendingTopics(1, 9), nil)
	assertStrings(t, "after", s.GetTrendingTopics(11, 99), nil)
	assertStrings(t, "around", s.GetTrendingTopics(1, 99), []string{"a"})
}

func TestTrendingOnEmptyService(t *testing.T) {
	s := NewService()
	assertStrings(t, "empty service", s.GetTrendingTopics(0, 1e12), nil)
}

func TestPartialWindowCountsOnlyPostsInside(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	addPosts(t, s, "ada", 1, "#x #y", "#x", "#y", "#y") // ts 1..4
	assertStrings(t, "2-4", s.GetTrendingTopics(2, 4), []string{"y", "x"})
	assertStrings(t, "3-4", s.GetTrendingTopics(3, 4), []string{"y"})
}

// ----------------------------------------------------------------------
// text storage (packed byte chunks; every string must round-trip exactly)

func TestMultibyteUnicodeRoundtrip(t *testing.T) {
	s := NewService()
	mustAddUser(t, s, "ada")
	texts := []string{
		"Åland café Ж中文 #tagged 🦀 done",
		strings.Repeat("🦀", 140), // 560 UTF-8 bytes, the maximum
	}
	addPosts(t, s, "ada", 1, texts...)
	assertStrings(t, "roundtrip", mustPostsForUser(t, s, "ada"),
		[]string{texts[1], texts[0]})
	assertStrings(t, "topic lookup", s.GetPostsForTopic("tagged"),
		[]string{texts[0]})
}

func TestInvalidUTF8Roundtrips(t *testing.T) {
	// Go strings may hold arbitrary bytes; storage must be byte-exact.
	// (\xed\xa0\x80 is a UTF-8-encoded lone surrogate, the Python
	// original's surrogatepass worst case.)
	s := NewService()
	mustAddUser(t, s, "ada")
	text := "weird \xed\xa0\x80 char #odd"
	mustAddPost(t, s, "ada", text, 1)
	assertStrings(t, "roundtrip", mustPostsForUser(t, s, "ada"),
		[]string{text})
	assertStrings(t, "topic lookup", s.GetPostsForTopic("odd"),
		[]string{text})
}

func TestEmptyPostAtExactChunkBoundary(t *testing.T) {
	// Regression test: a zero-length text stored while the write
	// position sits exactly at the 1 MiB chunk boundary must not
	// produce a location pointing past the chunk. (The Python original
	// has this latent bug; the port fixes it in storeBytes.)
	s := NewService()
	mustAddUser(t, s, "ada")
	full := strings.Repeat("🦀", 140) // 560 bytes
	ts := int64(1)
	for range chunkSize / 560 { // 1872 posts: 1048320 of 1048576 bytes
		mustAddPost(t, s, "ada", full, ts)
		ts++
	}
	mustAddPost(t, s, "ada", strings.Repeat("🦀", 64), ts) // exactly fills
	ts++
	if s.chunkPos != chunkSize {
		t.Fatalf("test setup: chunkPos = %d, want %d", s.chunkPos, chunkSize)
	}
	mustAddPost(t, s, "ada", "", ts)
	posts := mustPostsForUser(t, s, "ada")
	if posts[0] != "" {
		t.Fatalf("newest post = %q, want empty", posts[0])
	}
	mustAddPost(t, s, "ada", "still #alive", ts+1)
	assertStrings(t, "service keeps working", s.GetPostsForTopic("alive"),
		[]string{"still #alive"})
}

// ----------------------------------------------------------------------
// compaction

func TestMassDeleteCompactsAndPreservesSurvivors(t *testing.T) {
	s := NewService()
	for _, name := range []string{"a", "b", "c"} {
		mustAddUser(t, s, name)
	}
	ts := int64(0)
	for i := 0; i < 200; i++ { // round-robin: posts interleave in the blob
		for _, name := range []string{"a", "b", "c"} {
			ts++
			mustAddPost(t, s, name,
				"user "+name+" number "+itoa(i)+" #tag"+itoa(i%7), ts)
		}
	}
	survivors := mustPostsForUser(t, s, "b")

	if err := s.DeleteUser("a"); err != nil {
		t.Fatalf("DeleteUser(a): %v", err)
	}
	if err := s.DeleteUser("c"); err != nil { // ~2/3 of text bytes now dead
		t.Fatalf("DeleteUser(c): %v", err)
	}

	// White-box: compaction must have run and reclaimed everything.
	if s.deadTextBytes != 0 {
		t.Fatalf("deadTextBytes = %d after mass delete, want 0",
			s.deadTextBytes)
	}
	assertStrings(t, "survivors intact", mustPostsForUser(t, s, "b"),
		survivors)
	var wantTag3 []string
	for _, text := range survivors {
		if strings.Contains(text, "#tag3") {
			wantTag3 = append(wantTag3, text)
		}
	}
	assertStrings(t, "tag3 posts", s.GetPostsForTopic("tag3"), wantTag3)
	// The service keeps accepting and serving posts after compaction.
	mustAddUser(t, s, "d")
	mustAddPost(t, s, "d", "fresh #tag3 post", ts+1)
	if got := s.GetPostsForTopic("tag3")[0]; got != "fresh #tag3 post" {
		t.Fatalf("newest tag3 post = %q", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

// ----------------------------------------------------------------------
// randomized cross-check against a naive reference model
//
// Drive the service and the model with the same random operations, then
// require identical answers to every query. The model stores plain
// (user, text, timestamp) tuples and recomputes every answer by brute
// force with a regexp parser, so it is obviously correct — any
// divergence points at the service's indexing, deletion or ordering.

var refTopicRE = regexp.MustCompile(`#([0-9a-zA-Z_]+)`)

var refVocab = []string{"alpha", "Alpha", "beta", "b2", "under_score",
	"_", "caf", "gamma", "delta", "epsilon"}

type refPost struct {
	user string
	text string
	ts   int64
}

func refParse(text string) map[string]bool {
	topics := make(map[string]bool)
	for _, m := range refTopicRE.FindAllStringSubmatch(text, -1) {
		topics[m[1]] = true
	}
	return topics
}

// posts is kept in ascending-timestamp order, so "descending timestamp"
// is a backwards walk.

func refPostsForUser(posts []refPost, user string) []string {
	var texts []string
	for i := len(posts) - 1; i >= 0; i-- {
		if posts[i].user == user {
			texts = append(texts, posts[i].text)
		}
	}
	return texts
}

func refPostsForTopic(posts []refPost, topic string) []string {
	var texts []string
	for i := len(posts) - 1; i >= 0; i-- {
		if refParse(posts[i].text)[topic] {
			texts = append(texts, posts[i].text)
		}
	}
	return texts
}

func refTrending(posts []refPost, lo, hi int64) []string {
	counts := make(map[string]int)
	for _, p := range posts {
		if lo <= p.ts && p.ts <= hi {
			for topic := range refParse(p.text) {
				counts[topic]++
			}
		}
	}
	topics := make([]string, 0, len(counts))
	for topic := range counts {
		topics = append(topics, topic)
	}
	sort.Slice(topics, func(i, j int) bool {
		if counts[topics[i]] != counts[topics[j]] {
			return counts[topics[i]] > counts[topics[j]]
		}
		return topics[i] < topics[j]
	})
	return topics
}

type refState struct {
	service    *Service
	posts      []refPost // live posts only
	registered map[string]bool
	allNames   []string
	timestamp  int64
	rng        *rand.Rand
}

// runRandomOps drives a random AddUser/AddPost/DeleteUser mix
// (remainder deletes); invalid calls are asserted to fail. Returns the
// final state.
func runRandomOps(t *testing.T, seed uint64, nOps int,
	pAddUser, pAddPost float64) *refState {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0))
	st := &refState{
		service:    NewService(),
		registered: make(map[string]bool),
		rng:        rng,
	}
	for i := 0; i < 12; i++ {
		st.allNames = append(st.allNames, "user"+itoa(i))
	}
	words := []string{"lorem", "ipsum", "nice", "day", "again", "ugh",
		"#", "##"}

	randomText := func() string {
		perm := rng.Perm(len(words))
		pieces := make([]string, 0, 7)
		for _, idx := range perm[:1+rng.IntN(4)] {
			pieces = append(pieces, words[idx])
		}
		vocab := append(slices.Clone(refVocab), "café")
		for range rng.IntN(4) {
			pieces = append(pieces, "#"+vocab[rng.IntN(len(vocab))])
		}
		rng.Shuffle(len(pieces), func(i, j int) {
			pieces[i], pieces[j] = pieces[j], pieces[i]
		})
		return strings.Join(pieces, " ")
	}

	for i := 0; i < nOps; i++ {
		op := rng.Float64()
		name := st.allNames[rng.IntN(len(st.allNames))]
		switch {
		case op < pAddUser:
			if st.registered[name] {
				assertError(t, "duplicate AddUser", st.service.AddUser(name))
			} else {
				mustAddUser(t, st.service, name)
				st.registered[name] = true
			}
		case op < pAddUser+pAddPost:
			text := randomText()
			if len([]rune(text)) > MaxPostLength {
				t.Fatalf("generated text too long: %q", text)
			}
			st.timestamp += 1 + rng.Int64N(5)
			if st.registered[name] {
				mustAddPost(t, st.service, name, text, st.timestamp)
				st.posts = append(st.posts,
					refPost{name, text, st.timestamp})
			} else {
				assertError(t, "AddPost for unknown user",
					st.service.AddPost(name, text, st.timestamp))
				st.timestamp -= rng.Int64N(2) // failed ts may be reused
			}
		default:
			if st.registered[name] {
				if err := st.service.DeleteUser(name); err != nil {
					t.Fatalf("DeleteUser(%q): %v", name, err)
				}
				delete(st.registered, name)
				st.posts = slices.DeleteFunc(st.posts,
					func(p refPost) bool { return p.user == name })
			} else {
				assertError(t, "DeleteUser of unknown user",
					st.service.DeleteUser(name))
			}
		}
	}
	return st
}

func checkAgainstModel(t *testing.T, st *refState) {
	t.Helper()
	for _, name := range st.allNames {
		if st.registered[name] {
			assertStrings(t, "posts for "+name,
				mustPostsForUser(t, st.service, name),
				refPostsForUser(st.posts, name))
		} else {
			_, err := st.service.GetPostsForUser(name)
			assertError(t, "posts for unregistered "+name, err)
		}
	}

	for _, topic := range append(slices.Clone(refVocab), "café", "missing") {
		assertStrings(t, "posts for topic "+topic,
			st.service.GetPostsForTopic(topic),
			refPostsForTopic(st.posts, topic))
	}

	windows := [][2]int64{
		{0, st.timestamp}, {st.timestamp, 0}, {-5, -1},
		{st.timestamp + 1, st.timestamp + 100},
	}
	for i := 0; i < 30; i++ {
		a := st.rng.Int64N(st.timestamp + 2)
		b := st.rng.Int64N(st.timestamp + 2)
		windows = append(windows, [2]int64{min(a, b), max(a, b)})
	}
	for i := 0; i < 10; i++ { // some inverted
		windows = append(windows, [2]int64{
			st.rng.Int64N(st.timestamp + 1),
			st.rng.Int64N(st.timestamp + 1),
		})
	}
	for _, w := range windows {
		assertStrings(t, "trending",
			st.service.GetTrendingTopics(w[0], w[1]),
			refTrending(st.posts, w[0], w[1]))
	}
}

func TestRandomizedOperationsMatchReferenceModel(t *testing.T) {
	st := runRandomOps(t, 20261022, 600, 0.10, 0.70)
	checkAgainstModel(t, st)
}

func TestDeleteHeavyChurnMatchesModelAndCompacts(t *testing.T) {
	// 40% deletions force repeated text-blob compactions mid-run while
	// the model keeps checking every answer.
	st := runRandomOps(t, 7, 800, 0.25, 0.35)
	if st.service.compactions == 0 {
		t.Fatal("the delete-heavy scenario never triggered compaction")
	}
	if st.service.deadTextBytes > st.service.liveTextBytes {
		t.Fatalf("invariant violated: deadTextBytes %d > liveTextBytes %d",
			st.service.deadTextBytes, st.service.liveTextBytes)
	}
	checkAgainstModel(t, st)
}

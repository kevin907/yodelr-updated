// In-memory implementation of the Yodelr service.
//
// Designed and measured for 1-50+ million posts on a single machine.
// Two ideas keep memory flat and queries fast at that scale:
//
//  1. Columnar storage — a post is a dense integer id; everything about
//     it lives at that index in parallel slices. No per-post heap
//     objects: post texts are packed back-to-back into shared 1 MiB
//     byte buffers and addressed by offset/length slices (~10 bytes per
//     post instead of a 16-byte string header + heap allocation each),
//     copied back out to a string only when a query returns them.
//
//  2. Post-id order == timestamp order. The spec guarantees AddPost
//     timestamps increase strictly monotonically (enforced here, since
//     every index silently depends on it), so ids assigned in insertion
//     order are already time-sorted:
//
//     - GetPostsForUser / GetPostsForTopic walk a per-user / per-topic
//       id slice backwards — O(result).
//     - GetTrendingTopics binary-searches the timestamp slice for the
//       post-id window, then counts one contiguous span of the mention
//       log — O(log n + mentions in range + u*log u) for u unique
//       topics in the window.

package yodelr

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxPostLength is the maximum post length in Unicode code points.
const MaxPostLength = 140

// Timestamps are int64 by signature, so unlike the Python original
// (arbitrary-precision ints checked against the storage width) no range
// validation is needed: every representable argument is storable.

// deletedTopic is written over a deleted post's entries in the mention
// log. Real topic ids are always >= 0, so counting a range and dropping
// deletedTopic afterwards excludes deleted posts in O(1) instead of a
// per-post liveness check.
const deletedTopic int32 = -1

// Text blob layout: texts live in fixed-size byte chunks and a post's
// location is (chunk index << chunkBits) | offset. A post is at most
// 140 code points <= 560 UTF-8 bytes, so every post fits in one chunk.
const (
	chunkBits = 20
	chunkSize = 1 << chunkBits // 1 MiB
	chunkMask = chunkSize - 1
)

// deadLoc is the location sentinel for a deleted post.
const deadLoc = math.MaxUint64

// Service is a single-threaded, memory-conscious Yodelr service.
//
// Storage layout (post id == index, assigned in timestamp order):
//
//   - textChunks + textLoc + textLen — post texts packed back-to-back
//     as UTF-8 in 1 MiB buffers; loc/len address each post's bytes.
//     Deleting zeroes the bytes in place; once more than half of all
//     stored text bytes are dead, one compaction pass rebuilds the
//     buffers (amortized O(1) per delete).
//   - timestamps — int64, strictly increasing.
//   - mentionFlat / mentionOff — per-post topic ids packed in CSR
//     form: post p's topic ids live in
//     mentionFlat[mentionOff[p]:mentionOff[p+1]].
//   - topicIDs / topicNames — topic string interning.
//   - topicPosts — per-topic ascending slice of live post ids.
//   - userPosts — per-user ascending slice of that user's post ids; a
//     user's posts are only tombstoned when the user is deleted, so
//     these ids are always live.
//
// int32 post ids support ~2.1 billion posts over the service lifetime
// and fail loudly (an explicit error before any mutation) beyond that.
// Texts are stored as raw bytes, so every Go string — valid UTF-8 or
// not — round-trips exactly.
type Service struct {
	textChunks    [][]byte
	chunkPos      int // write offset in the last chunk
	textLoc       []uint64
	textLen       []uint16
	liveTextBytes int64
	deadTextBytes int64
	livePostCount int
	timestamps    []int64
	mentionFlat   []int32
	mentionOff    []int64
	topicIDs      map[string]int32
	topicNames    []string
	topicPosts    [][]int32
	userPosts     map[string][]int32
	compactions   int // times compactText has run (stats/tests only)
}

var _ Yodelr = (*Service)(nil)

// NewService returns an empty Yodelr service.
func NewService() *Service {
	return &Service{
		chunkPos:   chunkSize,
		mentionOff: []int64{0},
		topicIDs:   make(map[string]int32),
		userPosts:  make(map[string][]int32),
	}
}

// ----------------------------------------------------------------------
// text blob helpers

// storeBytes appends encoded text to the blob and returns its packed
// location. A fresh chunk is started when the text does not fit in the
// current one — and also when the write position sits exactly at the
// chunk boundary, so a zero-length text never gets a location whose
// offset overflows into the chunk-index bits.
func (s *Service) storeBytes(data []byte) uint64 {
	size := len(data)
	pos := s.chunkPos
	if len(s.textChunks) == 0 || pos+size > chunkSize || pos == chunkSize {
		s.textChunks = append(s.textChunks, make([]byte, chunkSize))
		pos = 0
	}
	copy(s.textChunks[len(s.textChunks)-1][pos:], data)
	s.chunkPos = pos + size
	return uint64(len(s.textChunks)-1)<<chunkBits | uint64(pos)
}

func (s *Service) decodeText(postID int32) string {
	loc := s.textLoc[postID]
	pos := loc & chunkMask
	raw := s.textChunks[loc>>chunkBits]
	return string(raw[pos : pos+uint64(s.textLen[postID])])
}

// compactText rebuilds the text blob with live posts only, reclaiming
// the space of deleted ones. O(total live bytes); triggered only when
// more than half of all stored bytes are dead, so the cost is amortized
// O(1) per deleted post. Post ids are unaffected.
func (s *Service) compactText() {
	oldChunks := s.textChunks
	s.textChunks = nil
	s.chunkPos = chunkSize
	for postID := range s.textLoc {
		loc := s.textLoc[postID]
		if loc == deadLoc {
			continue
		}
		pos := loc & chunkMask
		size := uint64(s.textLen[postID])
		chunk := oldChunks[loc>>chunkBits]
		s.textLoc[postID] = s.storeBytes(chunk[pos : pos+size])
	}
	s.deadTextBytes = 0
	s.compactions++
}

// ----------------------------------------------------------------------
// topic parsing

func isTopicChar(c byte) bool {
	return c == '_' ||
		'0' <= c && c <= '9' ||
		'a' <= c && c <= 'z' ||
		'A' <= c && c <= 'Z'
}

// appendTopics appends to dst each topic in text — '#' followed by one
// or more characters from [0-9a-zA-Z_], exactly the spec's charset —
// deduplicated in first-seen order, and returns the extended slice.
// The returned strings alias text's bytes; clone before retaining.
func appendTopics(dst []string, text string) []string {
	start := len(dst)
	for i := 0; i < len(text); {
		if text[i] != '#' {
			i++
			continue
		}
		j := i + 1
		for j < len(text) && isTopicChar(text[j]) {
			j++
		}
		if j > i+1 {
			topic := text[i+1 : j]
			if !slices.Contains(dst[start:], topic) {
				dst = append(dst, topic)
			}
		}
		i = j
	}
	return dst
}

// ----------------------------------------------------------------------
// read-only statistics (extensions beyond the Yodelr interface, which
// the brief explicitly allows as a superset)

// UserCount returns the number of registered users. O(1).
func (s *Service) UserCount() int { return len(s.userPosts) }

// PostCount returns the number of live (non-deleted) posts. O(1).
func (s *Service) PostCount() int { return s.livePostCount }

// TopicCount returns the number of distinct topics with at least one
// live post. O(distinct topics ever seen) — a stats call, not a hot
// path.
func (s *Service) TopicCount() int {
	n := 0
	for _, posts := range s.topicPosts {
		if len(posts) > 0 {
			n++
		}
	}
	return n
}

// TopicPostCount returns the number of live posts mentioning the topic
// without materializing their texts. O(1).
func (s *Service) TopicPostCount(topic string) int {
	id, ok := s.topicIDs[topic]
	if !ok {
		return 0
	}
	return len(s.topicPosts[id])
}

// ----------------------------------------------------------------------
// Yodelr API

// AddUser registers a user. It fails if the name is taken.
func (s *Service) AddUser(userName string) error {
	if _, ok := s.userPosts[userName]; ok {
		return fmt.Errorf("user already exists: '%s'", userName)
	}
	s.userPosts[userName] = nil
	return nil
}

// AddPost stores a post for a registered user.
//
// It fails for an unknown user, a text longer than 140 characters
// (Unicode code points), or a timestamp that is not strictly greater
// than the previous post's. Validation happens before any mutation, so
// a failed call leaves no trace.
func (s *Service) AddPost(userName, postText string, timestamp int64) error {
	userPosts, ok := s.userPosts[userName]
	if !ok {
		return fmt.Errorf("unknown user: '%s'", userName)
	}
	// A string under 140 bytes cannot exceed 140 code points; count
	// runes only when it might. Invalid UTF-8 counts one per byte.
	if len(postText) > MaxPostLength {
		if n := utf8.RuneCountInString(postText); n > MaxPostLength {
			return fmt.Errorf("post is %d characters, limit is %d",
				n, MaxPostLength)
		}
	}
	if n := len(s.timestamps); n > 0 && timestamp <= s.timestamps[n-1] {
		return fmt.Errorf("timestamp %d is not greater than the previous "+
			"post's (%d); timestamps must be unique and monotonically "+
			"increasing", timestamp, s.timestamps[n-1])
	}
	if len(s.textLoc) > math.MaxInt32 {
		return fmt.Errorf("post id space exhausted: the service stores at "+
			"most %d posts over its lifetime", int64(math.MaxInt32)+1)
	}

	postID := int32(len(s.textLoc))
	s.textLoc = append(s.textLoc, s.storeBytes([]byte(postText)))
	s.textLen = append(s.textLen, uint16(len(postText)))
	s.liveTextBytes += int64(len(postText))
	s.timestamps = append(s.timestamps, timestamp)
	var topicBuf [MaxPostLength / 2]string // a topic takes >= 2 chars
	for _, topic := range appendTopics(topicBuf[:0], postText) {
		topicID, ok := s.topicIDs[topic]
		if !ok {
			topicID = int32(len(s.topicNames))
			topic = strings.Clone(topic) // don't pin the whole post text
			s.topicIDs[topic] = topicID
			s.topicNames = append(s.topicNames, topic)
			s.topicPosts = append(s.topicPosts, nil)
		}
		s.mentionFlat = append(s.mentionFlat, topicID)
		s.topicPosts[topicID] = append(s.topicPosts[topicID], postID)
	}
	s.mentionOff = append(s.mentionOff, int64(len(s.mentionFlat)))
	s.userPosts[userName] = append(userPosts, postID)
	s.livePostCount++
	return nil
}

// DeleteUser deletes a user and all their posts. It fails if the user
// is unknown.
//
// The posts' text bytes are zeroed immediately (nothing readable
// remains) and their blob space is reclaimed by compaction once it
// exceeds half of the stored bytes. Each affected topic's index is
// rebuilt once without the deleted ids. Cost is O(user's mentions +
// size of affected topic indexes + amortized compaction); deletions are
// the rare operation, so the price is paid here rather than on every
// query.
func (s *Service) DeleteUser(userName string) error {
	userPosts, ok := s.userPosts[userName]
	if !ok {
		return fmt.Errorf("unknown user: '%s'", userName)
	}
	delete(s.userPosts, userName)

	s.livePostCount -= len(userPosts)
	deleted := make(map[int32]struct{}, len(userPosts))
	affectedTopics := make(map[int32]struct{})
	for _, postID := range userPosts {
		deleted[postID] = struct{}{}
		loc := s.textLoc[postID]
		size := uint64(s.textLen[postID])
		if size > 0 {
			pos := loc & chunkMask
			clear(s.textChunks[loc>>chunkBits][pos : pos+size])
		}
		s.textLoc[postID] = deadLoc
		s.textLen[postID] = 0
		s.liveTextBytes -= int64(size)
		s.deadTextBytes += int64(size)
		for k := s.mentionOff[postID]; k < s.mentionOff[postID+1]; k++ {
			affectedTopics[s.mentionFlat[k]] = struct{}{}
			s.mentionFlat[k] = deletedTopic
		}
	}
	for topicID := range affectedTopics {
		s.topicPosts[topicID] = slices.DeleteFunc(s.topicPosts[topicID],
			func(p int32) bool { _, dead := deleted[p]; return dead })
	}
	if s.deadTextBytes > s.liveTextBytes {
		s.compactText()
	}
	return nil
}

// GetPostsForUser returns all of a user's post texts, newest first. It
// fails if the user is unknown.
func (s *Service) GetPostsForUser(userName string) ([]string, error) {
	userPosts, ok := s.userPosts[userName]
	if !ok {
		return nil, fmt.Errorf("unknown user: '%s'", userName)
	}
	return s.decodeNewestFirst(userPosts), nil
}

// GetPostsForTopic returns all post texts mentioning the topic, newest
// first. An unknown topic yields an empty result.
func (s *Service) GetPostsForTopic(topic string) []string {
	topicID, ok := s.topicIDs[topic]
	if !ok {
		return []string{}
	}
	return s.decodeNewestFirst(s.topicPosts[topicID])
}

// decodeNewestFirst decodes ascending post ids into texts, newest
// first. Ids ascend in time, so walking backwards is already the
// required descending-timestamp order.
func (s *Service) decodeNewestFirst(postIDs []int32) []string {
	texts := make([]string, len(postIDs))
	for i, postID := range postIDs {
		texts[len(postIDs)-1-i] = s.decodeText(postID)
	}
	return texts
}

// GetTrendingTopics returns the topics mentioned in [from, to]
// (inclusive), most mentioned first, ties broken alphabetically. Empty
// or inverted intervals yield an empty result.
func (s *Service) GetTrendingTopics(fromTimestamp, toTimestamp int64) []string {
	// lo is the first post id with timestamp >= from; hi the first with
	// timestamp > to (timestamps are unique, so bisect_right is
	// bisect_left plus one when the bound itself is present).
	ts := s.timestamps
	lo, _ := slices.BinarySearch(ts, fromTimestamp)
	hi, found := slices.BinarySearch(ts, toTimestamp)
	if found {
		hi++
	}
	if lo >= hi {
		return []string{}
	}
	// Posts lo..hi-1 occupy one contiguous span of the mention log;
	// count it in one pass.
	counts := make(map[int32]int64)
	for _, topicID := range s.mentionFlat[s.mentionOff[lo]:s.mentionOff[hi]] {
		counts[topicID]++
	}
	delete(counts, deletedTopic)
	type topicCount struct {
		id    int32
		count int64
	}
	ranked := make([]topicCount, 0, len(counts))
	for id, count := range counts {
		ranked = append(ranked, topicCount{id, count})
	}
	names := s.topicNames
	slices.SortFunc(ranked, func(a, b topicCount) int {
		if c := cmp.Compare(b.count, a.count); c != 0 {
			return c
		}
		return strings.Compare(names[a.id], names[b.id])
	})
	topics := make([]string, len(ranked))
	for i, tc := range ranked {
		topics[i] = names[tc.id]
	}
	return topics
}

// Package yodelr implements the Yodelr micro-message service: users
// post messages of up to 140 characters carrying #topics, and the
// service answers per-user, per-topic and trending-topic queries.
//
// Yodelr is the service interface, Service the in-memory
// implementation. This is a Go port of the Python solution; where
// Python signalled failures with ValueError, methods here return an
// error, and pure queries that could not fail there return no error
// here either.
package yodelr

// Yodelr is the service interface.
//
// It allows adding and deleting users, adding and retrieving posts and
// getting trending topics.
type Yodelr interface {
	// AddUser adds a user with the given username to the system.
	AddUser(userName string) error

	// AddPost adds a post for the given user with the given post text
	// and timestamp. The text may contain zero or more topics: '#'
	// followed by one or more characters in [0-9a-zA-Z_]. Timestamps
	// are seconds since an arbitrary epoch and must be unique and
	// monotonically increasing between invocations.
	AddPost(userName, postText string, timestamp int64) error

	// DeleteUser deletes the given user and all posts made by the user
	// from the system.
	DeleteUser(userName string) error

	// GetPostsForUser returns all post texts made by the given user,
	// sorted on descending timestamp.
	GetPostsForUser(userName string) ([]string, error)

	// GetPostsForTopic returns all post texts containing the given
	// topic, sorted on descending timestamp. An unknown topic yields
	// an empty result.
	GetPostsForTopic(topic string) []string

	// GetTrendingTopics returns all unique topics mentioned in posts
	// made in the inclusive interval [from, to], sorted on descending
	// mention count primarily, alphabetically secondarily. The mention
	// count for a topic is the number of posts that mentioned it in
	// the interval. Empty or inverted intervals yield an empty result.
	GetTrendingTopics(fromTimestamp, toTimestamp int64) []string
}

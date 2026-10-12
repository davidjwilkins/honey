package events

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLog(max int) (*Log, *time.Time) {
	l := New(max)
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestRecentIsNewestFirst(t *testing.T) {
	l, now := newLog(3)
	for i := 1; i <= 5; i++ {
		*now = now.Add(time.Second)
		l.Add(Warm, "", 0, Text(fmt.Sprint("event ", i)))
	}
	recent := l.Recent(10)
	require.Len(t, recent, 3, "only the latest are kept")
	assert.Equal(t, "event 5", recent[0].Text)
	assert.Equal(t, "event 3", recent[2].Text)
	assert.Equal(t, Warm, recent[0].Kind)
	assert.Len(t, l.Recent(2), 2)
}

func TestRepeatsAreCombined(t *testing.T) {
	l, now := newLog(10)
	l.Add(Error, "backend 502", time.Minute, Text("502 for /a/"))
	l.Add(Notice, "", 0, Text("something else"))
	*now = now.Add(30 * time.Second)
	l.Add(Error, "backend 502", time.Minute, Text("502 for /b/"))

	recent := l.Recent(10)
	require.Len(t, recent, 2)
	assert.Equal(t, "502 for /b/", recent[0].Text, "the repeat becomes the latest")
	assert.Equal(t, 2, recent[0].Count)
	assert.Equal(t, *now, recent[0].Time)

	*now = now.Add(2 * time.Minute)
	l.Add(Error, "backend 502", time.Minute, Text("502 for /c/"))
	assert.Len(t, l.Recent(10), 3, "but not once the window has passed")
	assert.Equal(t, 1, l.Recent(1)[0].Count)
}

func TestUpdatesBuildOnTheirData(t *testing.T) {
	l, _ := newLog(10)
	for _, path := range []string{"/", "/blog/", "/feed/"} {
		l.Add(Purge, "wordpress", time.Second, func(e *Event) {
			paths, _ := e.Data().([]string)
			paths = append(paths, path)
			e.SetData(paths)
			e.Text = "purged " + strings.Join(paths, ", ")
		})
	}
	require.Len(t, l.Recent(10), 1)
	assert.Equal(t, "purged /, /blog/, /feed/", l.Recent(1)[0].Text)
}

func TestNilLogDoesNothing(t *testing.T) {
	var l *Log
	l.Add(Warm, "", 0, Text("x"))
	assert.Empty(t, l.Recent(5))
}

func TestConcurrentUse(t *testing.T) {
	l := New(50)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Add(Error, fmt.Sprint(i%3), time.Minute, Text("x"))
				l.Recent(5)
			}
		}(i)
	}
	wg.Wait()
	assert.Len(t, l.Recent(100), 3)
}

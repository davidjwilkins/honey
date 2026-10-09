package singleflight

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestJoinStartsOneFlightPerKey(t *testing.T) {
	var g Group
	a, leader := g.Join("a")
	assert.True(t, leader)
	again, leader := g.Join("a")
	assert.False(t, leader)
	assert.Same(t, a, again)
	b, leader := g.Join("b")
	assert.True(t, leader)
	assert.NotSame(t, a, b)
	assert.True(t, g.InFlight("a"))
	assert.False(t, g.InFlight("c"))
}

func TestFinishReleasesWaiters(t *testing.T) {
	var g Group
	f, _ := g.Join("a")
	var wg sync.WaitGroup
	results := make([]Result, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			waiter, leader := g.Join("a")
			assert.False(t, leader)
			<-waiter.Done()
			results[i] = waiter.Result()
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	g.Finish("a", f, Result{StatusCode: 502})
	wg.Wait()
	for _, result := range results {
		assert.Equal(t, 502, result.StatusCode)
	}
	assert.False(t, g.InFlight("a"), "a finished Flight should be removed")
}

func TestFinishOnlyTakesEffectOnce(t *testing.T) {
	var g Group
	f, _ := g.Join("a")
	g.Finish("a", f, Result{Bypass: true})
	g.Finish("a", f, Result{Retry: true})
	assert.Equal(t, Result{Bypass: true}, f.Result())
}

func TestFinishDoesNotRemoveNewerFlight(t *testing.T) {
	var g Group
	old, _ := g.Join("a")
	g.Finish("a", old, Result{Retry: true})
	newer, leader := g.Join("a")
	assert.True(t, leader, "joining after a Flight finished starts a new one")
	g.Finish("a", old, Result{Retry: true})
	assert.True(t, g.InFlight("a"), "finishing an old Flight again mustn't remove the newer one")
	g.Finish("a", newer, Result{Retry: true})
	assert.False(t, g.InFlight("a"))
}

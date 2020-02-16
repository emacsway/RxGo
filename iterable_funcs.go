package rxgo

import (
	"sync"
)

type funcsIterable struct {
	f []Scatter
}

func newFuncsIterable(f ...Scatter) Iterable {
	return &funcsIterable{f: f}
}

func (i *funcsIterable) Observe(opts ...Option) <-chan Item {
	option := parseOptions(opts...)
	next := option.buildChannel()
	ctx := option.buildContext()

	wg := sync.WaitGroup{}
	done := func() {
		wg.Done()
	}
	for _, f := range i.f {
		wg.Add(1)
		go f(ctx, next, done)
	}
	go func() {
		wg.Wait()
		close(next)
	}()

	return next
}
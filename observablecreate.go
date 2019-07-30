package rxgo

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

func isClosed(ch <-chan interface{}) bool {
	select {
	case <-ch:
		return true
	default:
	}

	return false
}

func newDefaultObservable() *observable {
	return &observable{
		subscribeStrategy: coldSubscribe(),
		nextStrategy:      onNext(),
	}
}

// newColdObservableFromChannel creates an Observable from a given channel
func newColdObservableFromChannel(ch chan interface{}) Observable {
	obs := newDefaultObservable()
	obs.coldIterable = newIterableFromChannel(ch)
	return obs
}

// newColdObservableFromFunction creates a cold observable
func newColdObservableFromFunction(f func(chan interface{})) Observable {
	obs := newDefaultObservable()
	obs.coldIterable = newIterableFromFunc(f)
	return obs
}

func newHotObservableFromChannel(ch chan interface{}, opts ...Option) Observable {
	parsedOptions := ParseOptions(opts...)

	obs := newDefaultObservable()

	obs.hotObservers = make([]Observer, 0)
	obs.hotSubscribers = make([]chan<- interface{}, 0)
	obs.hotItemChannel = ch

	stategy := parsedOptions.BackpressureStrategy()
	switch stategy {
	default:
		panic(fmt.Sprintf("unknown stategy: %v", stategy))
	case None:
		obs.subscribeStrategy = hotSubscribeStrategyNoneBackPressure()
		go func() {
			for {
				if next, ok := <-obs.hotItemChannel; ok {
					obs.hotObserversMutex.Lock()
					for _, observer := range obs.hotObservers {
						observer.Handle(next)
					}
					obs.hotObserversMutex.Unlock()
				} else {
					return
				}
			}
		}()
	case Drop:
		panic("drop strategy not implemented yet")
	case Buffer:
		obs.subscribeStrategy = hotSubscribeStrategyBufferBackPressure(parsedOptions.Buffer())
		go func() {
			for {
				if next, ok := <-obs.hotItemChannel; ok {
					obs.hotObserversMutex.Lock()
					for _, ch := range obs.hotSubscribers {
						select {
						case ch <- next:
						default:
						}
					}
					obs.hotObserversMutex.Unlock()
				} else {
					return
				}
			}
		}()
	}

	return obs
}

// newObservableFromIterable creates an Observable from a given iterable
func newObservableFromIterable(it Iterable) Observable {
	obs := newDefaultObservable()
	obs.coldIterable = it
	return obs
}

// newObservableFromRange creates an Observable from a range.
func newObservableFromRange(start, count int) Observable {
	obs := newDefaultObservable()
	obs.coldIterable = newIterableFromRange(start, count)
	return obs
}

// newObservableFromSlice creates an Observable from a given channel
func newObservableFromSlice(s []interface{}) Observable {
	obs := newDefaultObservable()
	obs.coldIterable = newIterableFromSlice(s)
	return obs
}

// Amb take several Observables, emit all of the items from only the first of these Observables
// to emit an item or notification
func Amb(observable Observable, observables ...Observable) Observable {
	out := make(chan interface{})
	once := sync.Once{}

	f := func(o Observable) {
		it := o.Iterator(context.Background())
		item, err := it.Next(context.Background())
		once.Do(func() {
			if err == nil {
				out <- item
				for {
					if item, err := it.Next(context.Background()); err == nil {
						out <- item
					} else {
						close(out)
						return
					}
				}
			} else {
				close(out)
				return
			}
		})
	}

	go f(observable)
	for _, o := range observables {
		go f(o)
	}

	return newColdObservableFromChannel(out)
}

// CombineLatest combine the latest item emitted by each Observable via a specified function
// and emit items based on the results of this function
func CombineLatest(f FunctionN, observable Observable, observables ...Observable) Observable {
	out := make(chan interface{})
	go func() {
		var size = uint32(len(observables)) + 1
		var counter uint32
		s := make([]interface{}, size)
		cancels := make([]context.CancelFunc, size)
		mutex := sync.Mutex{}
		wg := sync.WaitGroup{}
		wg.Add(int(size))
		errCh := make(chan interface{})

		handler := func(it Iterator, i int) {
			for {
				if item, err := it.Next(context.Background()); err == nil {
					switch v := item.(type) {
					case error:
						out <- v
						errCh <- nil
						wg.Done()
						return
					default:
						if s[i] == nil {
							atomic.AddUint32(&counter, 1)
						}
						mutex.Lock()
						s[i] = v
						mutex.Unlock()
						if atomic.LoadUint32(&counter) == size {
							out <- f(s...)
						}
					}
				} else {
					wg.Done()
					return
				}
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		it := observable.Iterator(ctx)
		go handler(it, 0)
		cancels[0] = cancel
		for i, o := range observables {
			ctx, cancel := context.WithCancel(context.Background())
			it = o.Iterator(ctx)
			go handler(it, i+1)
			cancels[i+1] = cancel
		}

		go func() {
			for range errCh {
				for _, cancel := range cancels {
					cancel()
				}
			}
		}()

		wg.Wait()
		close(out)
	}()
	return newColdObservableFromChannel(out)
}

// Concat emit the emissions from two or more Observables without interleaving them
func Concat(observable1 Observable, observables ...Observable) Observable {
	out := make(chan interface{})
	go func() {
		it := observable1.Iterator(context.Background())
		for {
			if item, err := it.Next(context.Background()); err == nil {
				out <- item
			} else {
				break
			}
		}

		for _, obs := range observables {
			it := obs.Iterator(context.Background())
			for {
				if item, err := it.Next(context.Background()); err == nil {
					out <- item
				} else {
					break
				}
			}
		}

		close(out)
	}()
	return newColdObservableFromChannel(out)
}

// Create observable from based on source function. Keep it mind to call emitter.OnDone()
// to signal sequence's end.
// Example:
// - emitting none elements
// observable.Create(emitter observer.Observer, disposed bool) { emitter.OnDone() })
// - emitting one element
// observable.Create(func(emitter observer.Observer, disposed bool) {
//		emitter.OnNext("one element")
//		emitter.OnDone()
// })
func Create(source func(emitter Observer, disposed bool)) Observable {
	out := make(chan interface{})
	emitter := NewObserver(
		NextFunc(func(el interface{}) {
			if !isClosed(out) {
				out <- el
			}
		}), ErrFunc(func(err error) {
			// decide how to deal with errors
			if !isClosed(out) {
				close(out)
			}
		}), DoneFunc(func() {
			if !isClosed(out) {
				close(out)
			}
		}),
	)

	go func() {
		source(emitter, isClosed(out))
	}()

	return newColdObservableFromChannel(out)
}

// Empty creates an Observable with no item and terminate immediately.
func Empty() Observable {
	out := make(chan interface{})
	go func() {
		close(out)
	}()
	return newColdObservableFromChannel(out)
}

// Error returns an Observable that invokes an Observer's onError method
// when the Observer subscribes to it.
func Error(err error) Observable {
	return &observable{
		errorOnSubscription: err,
	}
}

// FromChannel creates a cold observable from a channel
func FromChannel(ch chan interface{}) Observable {
	return newColdObservableFromChannel(ch)
}

// FromEventSource creates a hot observable
func FromEventSource(ch chan interface{}, opts ...Option) Observable {
	return newHotObservableFromChannel(ch, opts...)
}

// FromIterable creates a cold observable from an iterable
func FromIterable(it Iterable) Observable {
	return newObservableFromIterable(it)
}

// FromIterator creates a new Observable from an Iterator.
func FromIterator(it Iterator) Observable {
	out := make(chan interface{})
	go func() {
		for {
			if item, err := it.Next(context.Background()); err == nil {
				out <- item
			} else {
				break
			}
		}
		close(out)
	}()
	return newColdObservableFromChannel(out)
}

// FromSlice creates a new Observable from a slice.
func FromSlice(s []interface{}) Observable {
	return newObservableFromSlice(s)
}

// Interval creates an Observable emitting incremental integers infinitely between
// each given time interval.
func Interval(ctx context.Context, interval time.Duration) Observable {
	out := make(chan interface{})
	go func() {
		i := 0
		for {
			select {
			case <-time.After(interval):
				out <- i
				i++
			case <-ctx.Done():
				close(out)
				return
			}
		}
	}()
	return newColdObservableFromChannel(out)
}

// Just creates an Observable with the provided item(s).
func Just(item interface{}, items ...interface{}) Observable {
	if len(items) > 0 {
		items = append([]interface{}{item}, items...)
	} else {
		items = []interface{}{item}
	}

	return newObservableFromSlice(items)
}

// Merge combines multiple Observables into one by merging their emissions
func Merge(observable Observable, observables ...Observable) Observable {
	out := make(chan interface{})
	wg := sync.WaitGroup{}

	f := func(o Observable) {
		for {
			it := o.Iterator(context.Background())
			if item, err := it.Next(context.Background()); err == nil {
				out <- item
			} else {
				wg.Done()
				break
			}
		}
	}

	wg.Add(1)
	go f(observable)
	for _, o := range observables {
		wg.Add(1)
		go f(o)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return newColdObservableFromChannel(out)
}

// Never create an Observable that emits no items and does not terminate
func Never() Observable {
	out := make(chan interface{})
	return newColdObservableFromChannel(out)
}

// Range creates an Observable that emits a particular range of sequential integers.
func Range(start, count int) (Observable, error) {
	if count < 0 {
		return nil, errors.Wrap(&IllegalInputError{}, "count must be positive")
	}
	if start+count-1 > math.MaxInt32 {
		return nil, errors.Wrap(&IllegalInputError{}, "max value is bigger than math.MaxInt32")
	}

	return newObservableFromRange(start, count), nil
}

// Start creates an Observable from one or more directive-like Supplier
// and emits the result of each operation asynchronously on a new Observable.
func Start(f Supplier, fs ...Supplier) Observable {
	if len(fs) > 0 {
		fs = append([]Supplier{f}, fs...)
	} else {
		fs = []Supplier{f}
	}

	out := make(chan interface{})

	var wg sync.WaitGroup
	for _, f := range fs {
		wg.Add(1)
		go func(f Supplier) {
			out <- f()
			wg.Done()
		}(f)
	}

	// Wait in another goroutine to not block
	go func() {
		wg.Wait()
		close(out)
	}()

	return newColdObservableFromChannel(out)
}

// Timer returns an Observable that emits an empty structure after a specified delay, and then completes.
func Timer(d Duration) Observable {
	out := make(chan interface{})
	go func() {
		if d == nil {
			time.Sleep(0)
		} else {
			time.Sleep(d.duration())
		}
		out <- struct{}{}
		close(out)
	}()
	return newColdObservableFromChannel(out)
}
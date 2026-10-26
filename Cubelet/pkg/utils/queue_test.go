// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestQueueDequeueEmpty(t *testing.T) {
	q := NewQueue[int]()
	if q.Dequeue() != nil {
		t.Fatalf("dequeue empty queue returns non-nil")
	}
}

func TestQueueList(t *testing.T) {
	q := NewQueue[string]()
	if q.Length() != 0 {
		t.Fatalf("empty queue has non-zero length")
	}

	s := "122"
	q.Enqueue(&s)
	if q.Length() != 1 {
		t.Fatalf("count of enqueue wrong, want %d, got %d.", 1, q.Length())
	}
}

func TestQueue_Length(t *testing.T) {
	q := NewQueue[string]()
	if q.Length() != 0 {
		t.Fatalf("empty queue has non-zero length")
	}

	s := "122"
	q.Enqueue(&s)
	if q.Length() != 1 {
		t.Fatalf("count of enqueue wrong, want %d, got %d.", 1, q.Length())
	}

	q.Dequeue()
	if q.Length() != 0 {
		t.Fatalf("count of dequeue wrong, want %d, got %d", 0, q.Length())
	}
}

type Tap struct {
	Index   int
	Name    string
	IP      net.IP
	IsUsing bool
	File    *os.File
}

func ExampleQueue() {
	q := NewQueue[Tap]()
	q.Enqueue(&Tap{Name: "1"})
	q.Enqueue(&Tap{Name: "2"})
	q.Enqueue(&Tap{Name: "3"})

	fmt.Println(q.Dequeue())
	fmt.Println(q.Dequeue())
	fmt.Println(q.Dequeue())

}

func BenchmarkQueue(b *testing.B) {
	SkipCI(b)

	length := 1 << 12
	inputs := make([]*Tap, 0, length)
	for i := 0; i < length; i++ {
		inputs = append(inputs, &Tap{Name: fmt.Sprintf("tap-%d", i+1)})
	}
	q := NewQueue[Tap]()
	b.ResetTimer()
	var idx int64

	for _, cpus := range []int{1, 4, 8} {
		runtime.GOMAXPROCS(cpus)
		b.Run(strconv.Itoa(cpus)+"c", func(b *testing.B) {
			b.ResetTimer()
			var c int64
			b.RunParallel(func(pb *testing.PB) {
				idx := atomic.AddInt64(&idx, 1)
				for pb.Next() {
					i := int(atomic.AddInt64(&c, 1)-1) % length
					v := inputs[i]
					if idx%2 == 0 {
						q.Enqueue(v)

						continue
					}
					q.Dequeue()
				}
			})
		})
	}
}

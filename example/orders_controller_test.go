package main

import (
	"sync"
	"testing"
)

// Concurrent handlers must not race on the store or hand out duplicate IDs
// (run with -race).
func TestOrderStore_Concurrent(t *testing.T) {
	const n = 50
	ids := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(4)
		go func() {
			defer wg.Done()
			c := &CreateOrderController{}
			c.In.Product = "p"
			if err := c.Handle(nil); err != nil {
				t.Error(err)
			}
			ids <- c.Out.ID
		}()
		go func() {
			defer wg.Done()
			_ = (&ListOrdersController{}).Handle(nil)
		}()
		go func() {
			defer wg.Done()
			c := &UpdateOrderController{}
			c.Params.ID = 1
			c.In.Status = "shipped"
			_ = c.Handle(nil)
		}()
		go func() {
			defer wg.Done()
			c := &DeleteOrderController{}
			c.Params.ID = 1000 + i // unknown: a not-found error
			_ = c.Handle(nil)
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[int]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate order ID %d", id)
		}
		seen[id] = true
	}
}

// An ID freed by a delete is not reused.
func TestOrderStore_IDNotReusedAfterDelete(t *testing.T) {
	c1 := &CreateOrderController{}
	if err := c1.Handle(nil); err != nil {
		t.Fatal(err)
	}
	d := &DeleteOrderController{}
	d.Params.ID = c1.Out.ID
	if err := d.Handle(nil); err != nil {
		t.Fatal(err)
	}
	c2 := &CreateOrderController{}
	if err := c2.Handle(nil); err != nil {
		t.Fatal(err)
	}
	if c2.Out.ID == c1.Out.ID {
		t.Errorf("ID %d reused after delete", c2.Out.ID)
	}
}

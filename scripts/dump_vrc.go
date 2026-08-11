package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: dump_vrc <db>")
		os.Exit(2)
	}
	path := os.Args[1]
	db, err := bolt.Open(path, 0444, &bolt.Options{ReadOnly: true, Timeout: 3 * time.Second})
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	defer db.Close()
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("volume_refcounts"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var m map[string]any
			_ = json.Unmarshal(v, &m)
			fmt.Printf("%s\t%s\n", string(k), string(v))
			return nil
		})
	})
}

// Command coldcheck serves the dairy cold-store verification platform.
//
//	-demo            seed an in-memory store with all verification scenarios
//	-dsn=...         PostgreSQL DSN (system of record)
//
// The server only ingests telemetry and manual records and presents
// evidence; it never controls refrigeration or advises on edibility.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"coldcheck/rules"
	"coldcheck/seed"
	"coldcheck/store"
	"coldcheck/web"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	demo := flag.Bool("demo", true, "seed in-memory demo store")
	dsn := flag.String("dsn", "", "PostgreSQL DSN; empty uses in-memory/demo store")
	window := flag.Duration("window", 4*time.Hour, "evaluation look-back window")
	flag.Parse()

	var st store.Store
	if *dsn != "" {
		pg, err := store.NewPostgres(*dsn)
		if err != nil {
			log.Fatalf("connect postgres: %v", err)
		}
		st = pg
		log.Print("using PostgreSQL store")
	} else {
		if *demo {
			sc := seed.Build(time.Now())
			st = sc.Memory
			log.Printf("demo seeded: %d air readings, %d raw door events, freeze plan %s",
				len(sc.Snapshot.Air), len(sc.Snapshot.RawDoorEvents), sc.PlanID)
		} else {
			st = store.NewMemory()
		}
	}

	app, err := web.NewApp(st, rules.DefaultParams(), *window)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           withLog(app.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	_ = context.Background
	log.Printf("coldcheck listening on %s (window %s)", *addr, *window)
	log.Fatal(srv.ListenAndServe())
}

func withLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.String())
		h.ServeHTTP(w, r)
	})
}

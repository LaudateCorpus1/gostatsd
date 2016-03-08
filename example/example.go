package main

import (
	"log"

	"github.com/jtblin/gostatsd/statsd"
	"github.com/jtblin/gostatsd/types"
)

func main() {
	f := func(m types.Metric) {
		log.Printf("%s", m)
	}
	r := statsd.MetricReceiver{Addr: ":8125", Namespace: "stats", Handler: statsd.HandlerFunc(f)}
	r.ListenAndReceive()
}
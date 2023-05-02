package main

import (
	"github.com/shapeshift/market-proxy/proxyd"
)

func main() {
	p := proxyd.New()
	<-p.Done()
}

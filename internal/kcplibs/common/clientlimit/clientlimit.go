package clientlimit

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"k8s.io/client-go/rest"
)

const DefaultQPS float32 = 50

const DefaultBurst = 100

func Apply(cfg *rest.Config, qps float32, burst int) *rest.Config {
	out := rest.CopyConfig(cfg)
	qps = opt.Positive(qps, DefaultQPS)
	burst = opt.Positive(burst, DefaultBurst)
	out.QPS = opt.Positive(out.QPS, qps)
	out.Burst = opt.Positive(out.Burst, burst)
	return out
}

package debug

import (
	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/DataDog/dd-trace-go.v1/profiler"
	"gopkg.in/urfave/cli.v1"
)

var (
	datadogProfilerFlag = cli.BoolFlag{
		Name:  "datadog.profiler",
		Usage: "Enable Datadog profiler",
	}
	datadogProfilerExpensiveFlag = cli.BoolFlag{
		Name:  "datadog.profiler.expensive",
		Usage: "Enable Datadog profiler expensive metrics",
	}
	datadogProfilerEnvironmentFlag = cli.StringFlag{
		Name:  "datadog.profiler.environment",
		Usage: "Set Datadog profiler environment",
		Value: "",
	}
	datadogProfilerTagsFlag = cli.StringFlag{
		Name:  "datadog.profiler.tags",
		Usage: "Comma-separated tags (key:value) attached to all measurements",
		Value: "",
	}
)

func init() {
	Flags = append(Flags,
		datadogProfilerFlag,
		datadogProfilerExpensiveFlag,
		datadogProfilerEnvironmentFlag,
		datadogProfilerTagsFlag,
	)
}

func StartDatadogProfiler(service string, env string, version string, tags string, expensive bool) {
	option := profiler.WithProfileTypes(
		profiler.CPUProfile,
		profiler.HeapProfile,
	)

	// The profiles below are disabled by default to keep overhead
	// low, but can be enabled as needed.
	if expensive {
		option = profiler.WithProfileTypes(
			profiler.CPUProfile,
			profiler.HeapProfile,
			profiler.BlockProfile,
			profiler.MutexProfile,
			profiler.GoroutineProfile,
		)
	}
	err := profiler.Start(profiler.WithService(service),
		profiler.WithEnv(env),
		profiler.WithVersion(version),
		profiler.WithTags(tags),
		option,
	)
	if err != nil {
		log.Error("Error starting Datadog profiler", "err", err)
	}
}

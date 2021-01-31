// +build windows

package collector

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	netclrEnabledCollectors = kingpin.Flag(
		"collector.netframework_clr.collectors-enabled",
		"Comma-separated list of netframework_clr child collectors to use.").
		Default(netclrAvailableCollectors()).String()
	netclrPrintCollectors = kingpin.Flag(
		"collector.netframework_clr.collectors-print",
		"If true, print available netframework_clr child collectors and exit.  Only displays if the netframework_clr collector is enabled.",
	).Bool()
	netclrWhitelist = kingpin.Flag(
		"collector.netframework_clr.whitelist",
		"Regexp of processes to include. Process name must both match whitelist and not match blacklist to be included.",
	).Default(".*").String()
	netclrBlacklist = kingpin.Flag(
		"collector.netframework_clr.blacklist",
		"Regexp of processes to exclude. Process name must both match whitelist and not match blacklist to be included.",
	).Default("").String()
)

type netclrCollectorFunc func(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error)

type netclrCollectorsMap map[string]netclrCollectorFunc

func (c *netclrCollector) getNETCLRCollectors() netclrCollectorsMap {
	collectors := make(netclrCollectorsMap)
	collectors["exceptions"] = c.collectExceptions
	collectors["interop"] = c.collectInterop
	collectors["jit"] = c.collectJit
	collectors["loading"] = c.collectLoading
	collectors["locksandthreads"] = c.collectLocksAndThreads
	collectors["memory"] = c.collectMemory
	collectors["remoting"] = c.collectRemoting
	collectors["security"] = c.collectSecurity

	return collectors
}

func netclrAvailableCollectors() string {
	return "exceptions,interop,jit,loading,locksandthreads,memory,remoting,security"
}

func netclrExpandEnabledCollectors(enabled string) []string {
	separated := strings.Split(enabled, ",")
	unique := map[string]bool{}
	for _, s := range separated {
		if s != "" {
			unique[s] = true
		}
	}
	result := make([]string, 0, len(unique))
	for s := range unique {
		result = append(result, s)
	}
	return result
}

func netclrGetPerfObjectName(collector string) string {
	switch collector {
	case "exceptions":
		return ".NET CLR Exceptions"
	case "interop":
		return ".NET CLR Interop"
	case "jit":
		return ".NET CLR Jit"
	case "loading":
		return ".NET CLR Loading"
	case "locksandthreads":
		return ".NET CLR LocksAndThreads"
	case "memory":
		return ".NET CLR Memory"
	case "remoting":
		return ".NET CLR Remoting"
	case "security":
		return ".NET CLR Security"
	default:
		return ""
	}
}

func init() {
	registerCollector("netframework_clr", newNETCLRCollector)
}

type netclrCollector struct {
	// meta
	netclrScrapeDurationDesc *prometheus.Desc
	netclrScrapeSuccessDesc  *prometheus.Desc

	// .NET CLR Exceptions metrics
	NumberofExcepsThrown *prometheus.Desc
	NumberofFilters      *prometheus.Desc
	NumberofFinallys     *prometheus.Desc
	ThrowToCatchDepth    *prometheus.Desc

	// .NET CLR Interop metrics
	NumberofCCWs        *prometheus.Desc
	Numberofmarshalling *prometheus.Desc
	NumberofStubs       *prometheus.Desc

	// .NET CLR Jit metrics
	NumberofMethodsJitted      *prometheus.Desc
	TimeinJit                  *prometheus.Desc
	StandardJitFailures        *prometheus.Desc
	TotalNumberofILBytesJitted *prometheus.Desc

	// .NET CLR Loading metrics
	BytesinLoaderHeap         *prometheus.Desc
	Currentappdomains         *prometheus.Desc
	CurrentAssemblies         *prometheus.Desc
	CurrentClassesLoaded      *prometheus.Desc
	TotalAppdomains           *prometheus.Desc
	Totalappdomainsunloaded   *prometheus.Desc
	TotalAssemblies           *prometheus.Desc
	TotalClassesLoaded        *prometheus.Desc
	TotalNumberofLoadFailures *prometheus.Desc

	// .NET CLR LocksAndThreads metrics
	CurrentQueueLength               *prometheus.Desc
	NumberofcurrentlogicalThreads    *prometheus.Desc
	NumberofcurrentphysicalThreads   *prometheus.Desc
	Numberofcurrentrecognizedthreads *prometheus.Desc
	Numberoftotalrecognizedthreads   *prometheus.Desc
	QueueLengthPeak                  *prometheus.Desc
	TotalNumberofContentions         *prometheus.Desc

	// .NET CLR Memory metrics
	AllocatedBytes            *prometheus.Desc
	FinalizationSurvivors     *prometheus.Desc
	HeapSize                  *prometheus.Desc
	PromotedBytes             *prometheus.Desc
	NumberGCHandles           *prometheus.Desc
	NumberCollections         *prometheus.Desc
	NumberInducedGC           *prometheus.Desc
	NumberofPinnedObjects     *prometheus.Desc
	NumberofSinkBlocksinuse   *prometheus.Desc
	NumberTotalCommittedBytes *prometheus.Desc
	NumberTotalreservedBytes  *prometheus.Desc
	TimeinGC                  *prometheus.Desc

	// .NET CLR Remoting metrics
	Channels                  *prometheus.Desc
	ContextBoundClassesLoaded *prometheus.Desc
	ContextBoundObjects       *prometheus.Desc
	ContextProxies            *prometheus.Desc
	Contexts                  *prometheus.Desc
	TotalRemoteCalls          *prometheus.Desc

	// .NET CLR Security metrics
	NumberLinkTimeChecks *prometheus.Desc
	TimeinRTchecks       *prometheus.Desc
	StackWalkDepth       *prometheus.Desc
	TotalRuntimeChecks   *prometheus.Desc

	// Process whitelist and blacklist regexp
	processWhitelistPattern *regexp.Regexp
	processBlacklistPattern *regexp.Regexp

	netclrCollectors            netclrCollectorsMap
	netclrChildCollectorFailure int
}

func newNETCLRCollector() (Collector, error) {
	const subsystem = "netframework_clr"

	enabled := netclrExpandEnabledCollectors(*netclrEnabledCollectors)
	perfCounters := make([]string, len(enabled))
	for _, c := range enabled {
		perfCounters = append(perfCounters, netclrGetPerfObjectName(c))
	}
	addPerfCounterDependencies(subsystem, perfCounters)

	const exceptionsSubsystem = subsystem + "exceptions"
	const interopSubsystem = subsystem + "interop"
	const jitSubsystem = subsystem + "jit"
	const loadingSubsystem = subsystem + "loading"
	const locksandthreadsSubsystem = subsystem + "locksandthreads"
	const memorySubsystem = subsystem + "memory"
	const remotingSubsystem = subsystem + "remoting"
	const securitySubsystem = subsystem + "security"
	netclrCollector := &netclrCollector{
		// meta
		netclrScrapeDurationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, subsystem, "collector_duration_seconds"),
			"windows_exporter: Duration of a netframework_clr child collection.",
			[]string{"collector"},
			nil,
		),
		netclrScrapeSuccessDesc: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, subsystem, "collector_success"),
			"windows_exporter: Whether a netframework_clr child collector was successful.",
			[]string{"collector"},
			nil,
		),

		// .NET CLR Exceptions metrics
		NumberofExcepsThrown: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, exceptionsSubsystem, "exceptions_thrown_total"),
			"Displays the total number of exceptions thrown since the application started. This includes both .NET exceptions and unmanaged exceptions that are converted into .NET exceptions.",
			[]string{"process"},
			nil,
		),
		NumberofFilters: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, exceptionsSubsystem, "exceptions_filters_total"),
			"Displays the total number of .NET exception filters executed. An exception filter evaluates regardless of whether an exception is handled.",
			[]string{"process"},
			nil,
		),
		NumberofFinallys: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, exceptionsSubsystem, "exceptions_finallys_total"),
			"Displays the total number of finally blocks executed. Only the finally blocks executed for an exception are counted; finally blocks on normal code paths are not counted by this counter.",
			[]string{"process"},
			nil,
		),
		ThrowToCatchDepth: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, exceptionsSubsystem, "throw_to_catch_depth_total"),
			"Displays the total number of stack frames traversed, from the frame that threw the exception to the frame that handled the exception.",
			[]string{"process"},
			nil,
		),

		// .NET CLR Interop metrics
		NumberofCCWs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, interopSubsystem, "com_callable_wrappers_total"),
			"Displays the current number of COM callable wrappers (CCWs). A CCW is a proxy for a managed object being referenced from an unmanaged COM client.",
			[]string{"process"},
			nil,
		),
		Numberofmarshalling: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, interopSubsystem, "interop_marshalling_total"),
			"Displays the total number of times arguments and return values have been marshaled from managed to unmanaged code, and vice versa, since the application started.",
			[]string{"process"},
			nil,
		),
		NumberofStubs: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, interopSubsystem, "interop_stubs_created_total"),
			"Displays the current number of stubs created by the common language runtime. Stubs are responsible for marshaling arguments and return values from managed to unmanaged code, and vice versa, during a COM interop call or a platform invoke call.",
			[]string{"process"},
			nil,
		),

		// .NET CLR Jit metrics
		NumberofMethodsJitted: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, jitSubsystem, "jit_methods_total"),
			"Displays the total number of methods JIT-compiled since the application started. This counter does not include pre-JIT-compiled methods.",
			[]string{"process"},
			nil,
		),
		TimeinJit: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, jitSubsystem, "jit_time_percent"),
			"Displays the percentage of time spent in JIT compilation. This counter is updated at the end of every JIT compilation phase. A JIT compilation phase occurs when a method and its dependencies are compiled.",
			[]string{"process"},
			nil,
		),
		StandardJitFailures: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, jitSubsystem, "jit_standard_failures_total"),
			"Displays the peak number of methods the JIT compiler has failed to compile since the application started. This failure can occur if the MSIL cannot be verified or if there is an internal error in the JIT compiler.",
			[]string{"process"},
			nil,
		),
		TotalNumberofILBytesJitted: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, jitSubsystem, "jit_il_bytes_total"),
			"Displays the total number of Microsoft intermediate language (MSIL) bytes compiled by the just-in-time (JIT) compiler since the application started",
			[]string{"process"},
			nil,
		),

		// .NET CLR Loading metrics
		BytesinLoaderHeap: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "loader_heap_size_bytes"),
			"Displays the current size, in bytes, of the memory committed by the class loader across all application domains. Committed memory is the physical space reserved in the disk paging file.",
			[]string{"process"},
			nil,
		),
		Currentappdomains: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "appdomains_loaded_current"),
			"Displays the current number of application domains loaded in this application.",
			[]string{"process"},
			nil,
		),
		CurrentAssemblies: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "assemblies_loaded_current"),
			"Displays the current number of assemblies loaded across all application domains in the currently running application. If the assembly is loaded as domain-neutral from multiple application domains, this counter is incremented only once.",
			[]string{"process"},
			nil,
		),
		CurrentClassesLoaded: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "classes_loaded_current"),
			"Displays the current number of classes loaded in all assemblies.",
			[]string{"process"},
			nil,
		),
		TotalAppdomains: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "appdomains_loaded_total"),
			"Displays the peak number of application domains loaded since the application started.",
			[]string{"process"},
			nil,
		),
		Totalappdomainsunloaded: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "appdomains_unloaded_total"),
			"Displays the total number of application domains unloaded since the application started. If an application domain is loaded and unloaded multiple times, this counter increments each time the application domain is unloaded.",
			[]string{"process"},
			nil,
		),
		TotalAssemblies: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "assemblies_loaded_total"),
			"Displays the total number of assemblies loaded since the application started. If the assembly is loaded as domain-neutral from multiple application domains, this counter is incremented only once.",
			[]string{"process"},
			nil,
		),
		TotalClassesLoaded: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "classes_loaded_total"),
			"Displays the cumulative number of classes loaded in all assemblies since the application started.",
			[]string{"process"},
			nil,
		),
		TotalNumberofLoadFailures: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, loadingSubsystem, "class_load_failures_total"),
			"Displays the peak number of classes that have failed to load since the application started.",
			[]string{"process"},
			nil,
		),

		// .NET CLR LocksAndThreads metrics
		CurrentQueueLength: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "current_queue_length"),
			"Displays the total number of threads that are currently waiting to acquire a managed lock in the application.",
			[]string{"process"},
			nil,
		),
		NumberofcurrentlogicalThreads: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "current_logical_threads"),
			"Displays the number of current managed thread objects in the application. This counter maintains the count of both running and stopped threads. ",
			[]string{"process"},
			nil,
		),
		NumberofcurrentphysicalThreads: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "physical_threads_current"),
			"Displays the number of native operating system threads created and owned by the common language runtime to act as underlying threads for managed thread objects. This counter's value does not include the threads used by the runtime in its internal operations; it is a subset of the threads in the operating system process.",
			[]string{"process"},
			nil,
		),
		Numberofcurrentrecognizedthreads: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "recognized_threads_current"),
			"Displays the number of threads that are currently recognized by the runtime. These threads are associated with a corresponding managed thread object. The runtime does not create these threads, but they have run inside the runtime at least once.",
			[]string{"process"},
			nil,
		),
		Numberoftotalrecognizedthreads: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "recognized_threads_total"),
			"Displays the total number of threads that have been recognized by the runtime since the application started. These threads are associated with a corresponding managed thread object. The runtime does not create these threads, but they have run inside the runtime at least once.",
			[]string{"process"},
			nil,
		),
		QueueLengthPeak: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "queue_length_total"),
			"Displays the total number of threads that waited to acquire a managed lock since the application started.",
			[]string{"process"},
			nil,
		),
		TotalNumberofContentions: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, locksandthreadsSubsystem, "contentions_total"),
			"Displays the total number of times that threads in the runtime have attempted to acquire a managed lock unsuccessfully.",
			[]string{"process"},
			nil,
		),

		// .NET CLR Memory metrics
		AllocatedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "allocated_bytes_total"),
			"Displays the total number of bytes allocated on the garbage collection heap.",
			[]string{"process"},
			nil,
		),
		FinalizationSurvivors: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "finalization_survivors"),
			"Displays the number of garbage-collected objects that survive a collection because they are waiting to be finalized.",
			[]string{"process"},
			nil,
		),
		HeapSize: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "heap_size_bytes"),
			"Displays the maximum bytes that can be allocated; it does not indicate the current number of bytes allocated.",
			[]string{"process", "area"},
			nil,
		),
		PromotedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "promoted_bytes"),
			"Displays the bytes that were promoted from the generation to the next one during the last GC. Memory is promoted when it survives a garbage collection.",
			[]string{"process", "area"},
			nil,
		),
		NumberGCHandles: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "number_gc_handles"),
			"Displays the current number of garbage collection handles in use. Garbage collection handles are handles to resources external to the common language runtime and the managed environment.",
			[]string{"process"},
			nil,
		),
		NumberCollections: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "collections_total"),
			"Displays the number of times the generation objects are garbage collected since the application started.",
			[]string{"process", "area"},
			nil,
		),
		NumberInducedGC: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "induced_gc_total"),
			"Displays the peak number of times garbage collection was performed because of an explicit call to GC.Collect.",
			[]string{"process"},
			nil,
		),
		NumberofPinnedObjects: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "number_pinned_objects"),
			"Displays the number of pinned objects encountered in the last garbage collection.",
			[]string{"process"},
			nil,
		),
		NumberofSinkBlocksinuse: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "number_sink_blocksinuse"),
			"Displays the current number of synchronization blocks in use. Synchronization blocks are per-object data structures allocated for storing synchronization information. They hold weak references to managed objects and must be scanned by the garbage collector.",
			[]string{"process"},
			nil,
		),
		NumberTotalCommittedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "committed_bytes"),
			"Displays the amount of virtual memory, in bytes, currently committed by the garbage collector. Committed memory is the physical memory for which space has been reserved in the disk paging file.",
			[]string{"process"},
			nil,
		),
		NumberTotalreservedBytes: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "reserved_bytes"),
			"Displays the amount of virtual memory, in bytes, currently reserved by the garbage collector. Reserved memory is the virtual memory space reserved for the application when no disk or main memory pages have been used.",
			[]string{"process"},
			nil,
		),
		TimeinGC: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, memorySubsystem, "gc_time_percent"),
			"Displays the percentage of time that was spent performing a garbage collection in the last sample.",
			[]string{"process"},
			nil,
		),

		// .NET CLR Remoting metrics
		Channels: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "channels_total"),
			"Displays the total number of remoting channels registered across all application domains since application started.",
			[]string{"process"},
			nil,
		),
		ContextBoundClassesLoaded: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "context_bound_classes_loaded"),
			"Displays the current number of context-bound classes that are loaded.",
			[]string{"process"},
			nil,
		),
		ContextBoundObjects: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "context_bound_objects_total"),
			"Displays the total number of context-bound objects allocated.",
			[]string{"process"},
			nil,
		),
		ContextProxies: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "context_proxies_total"),
			"Displays the total number of remoting proxy objects in this process since it started.",
			[]string{"process"},
			nil,
		),
		Contexts: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "contexts"),
			"Displays the current number of remoting contexts in the application.",
			[]string{"process"},
			nil,
		),
		TotalRemoteCalls: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, remotingSubsystem, "remote_calls_total"),
			"Displays the total number of remote procedure calls invoked since the application started.",
			[]string{"process"},
			nil,
		),

		// .NET CLR Security metrics
		NumberLinkTimeChecks: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, securitySubsystem, "link_time_checks_total"),
			"Displays the total number of link-time code access security checks since the application started.",
			[]string{"process"},
			nil,
		),
		TimeinRTchecks: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, securitySubsystem, "rt_checks_time_percent"),
			"Displays the percentage of time spent performing runtime code access security checks in the last sample.",
			[]string{"process"},
			nil,
		),
		StackWalkDepth: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, securitySubsystem, "stack_walk_depth"),
			"Displays the depth of the stack during that last runtime code access security check.",
			[]string{"process"},
			nil,
		),
		TotalRuntimeChecks: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, securitySubsystem, "runtime_checks_total"),
			"Displays the total number of runtime code access security checks performed since the application started.",
			[]string{"process"},
			nil,
		),

		// Process name whitelist and blacklist
		processWhitelistPattern: regexp.MustCompile(fmt.Sprintf("^(?:%s)$", *netclrWhitelist)),
		processBlacklistPattern: regexp.MustCompile(fmt.Sprintf("^(?:%s)$", *netclrBlacklist)),
	}

	netclrCollector.netclrCollectors = netclrCollector.getNETCLRCollectors()
	if *netclrPrintCollectors {
		fmt.Printf("Available .NET CLR sub-collectors:\n")
		for name := range netclrCollector.netclrCollectors {
			fmt.Printf(" - %s\n", name)
		}
		os.Exit(0)
	}

	return netclrCollector, nil
}

func (c *netclrCollector) execute(ctx *ScrapeContext, name string, fn netclrCollectorFunc, ch chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()

	begin := time.Now()
	_, err := fn(ctx, ch)
	duration := time.Since(begin)
	var success float64

	if err != nil {
		log.Errorf("netframework_clr sub-collector %s failed after %fs: %s", name, duration.Seconds(), err)
		success = 0
		c.netclrChildCollectorFailure++
	} else {
		log.Debugf("netframework_clr sub-collector %s succeeded after %fs.", name, duration.Seconds())
		success = 1
	}
	ch <- prometheus.MustNewConstMetric(
		c.netclrScrapeDurationDesc,
		prometheus.GaugeValue,
		duration.Seconds(),
		name,
	)
	ch <- prometheus.MustNewConstMetric(
		c.netclrScrapeSuccessDesc,
		prometheus.GaugeValue,
		success,
		name,
	)
}

func (c *netclrCollector) Collect(ctx *ScrapeContext, ch chan<- prometheus.Metric) error {
	wg := sync.WaitGroup{}

	c.netclrChildCollectorFailure = 0
	enabled := netclrExpandEnabledCollectors(*netclrEnabledCollectors)
	for _, name := range enabled {
		function := c.netclrCollectors[name]

		wg.Add(1)
		go c.execute(ctx, name, function, ch, &wg)
	}
	wg.Wait()

	// This should return an error if any collector encountered an error.
	if c.netclrChildCollectorFailure > 0 {
		return errors.New("at least one child collector failed")
	}
	return nil
}

func (c *netclrCollector) netclrMapProcessName(name string, nameCounts map[string]int) (string, bool) {
	// Skip the _Global_ instance since it's just a sum of the other instances.
	if name == "_Global_" {
		return name, false
	}

	// Append a "#1", "#2", etc., suffix if a process name appears more than once.
	// WMI does this automatically, but Perflib does not.
	procnum, exists := nameCounts[name]
	if exists {
		nameCounts[name]++
		name = fmt.Sprintf("%s#%d", name, procnum)
	} else {
		nameCounts[name] = 1
	}

	// The pattern matching against the whitelist and blacklist has to occur
	// after appending #N above to be consistent with other collectors.
	keep := !c.processBlacklistPattern.MatchString(name) && c.processWhitelistPattern.MatchString(name)
	return name, keep
}

type netclrExceptions struct {
	Name string

	NumberofExcepsThrown    float64 `perflib:"# of Exceps Thrown"`
	NumberofFiltersPersec   float64 `perflib:"# of Filters / sec"`
	NumberofFinallysPersec  float64 `perflib:"# of Finallys / sec"`
	ThrowToCatchDepthPersec float64 `perflib:"Throw To Catch Depth / sec"`
}

func (c *netclrCollector) collectExceptions(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrExceptions

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("exceptions")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.NumberofExcepsThrown,
			prometheus.CounterValue,
			process.NumberofExcepsThrown,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofFilters,
			prometheus.CounterValue,
			process.NumberofFiltersPersec,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofFinallys,
			prometheus.CounterValue,
			process.NumberofFinallysPersec,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.ThrowToCatchDepth,
			prometheus.CounterValue,
			process.ThrowToCatchDepthPersec,
			name,
		)
	}

	return nil, nil
}

type netclrInterop struct {
	Name string

	NumberofCCWs        float64 `perflib:"# of CCWs"`
	Numberofmarshalling float64 `perflib:"# of marshalling"`
	NumberofStubs       float64 `perflib:"# of Stubs"`
}

func (c *netclrCollector) collectInterop(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrInterop

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("interop")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.NumberofCCWs,
			prometheus.CounterValue,
			process.NumberofCCWs,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Numberofmarshalling,
			prometheus.CounterValue,
			process.Numberofmarshalling,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofStubs,
			prometheus.CounterValue,
			process.NumberofStubs,
			name,
		)
	}

	return nil, nil
}

type netclrJit struct {
	Name string

	Frequency_PerfTime         float64 `perflib:"Not Displayed_Base"`
	NumberofMethodsJitted      float64 `perflib:"# of Methods Jitted"`
	PercentTimeinJit           float64 `perflib:"% Time in Jit"`
	StandardJitFailures        float64 `perflib:"Standard Jit Failures"`
	TotalNumberofILBytesJitted float64 `perflib:"Total # of IL Bytes Jitted"`
}

func (c *netclrCollector) collectJit(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrJit

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("jit")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.NumberofMethodsJitted,
			prometheus.CounterValue,
			process.NumberofMethodsJitted,
			name,
		)

		timeInJit := 0.0
		if process.Frequency_PerfTime != 0 {
			timeInJit = process.PercentTimeinJit / process.Frequency_PerfTime
		}
		ch <- prometheus.MustNewConstMetric(
			c.TimeinJit,
			prometheus.GaugeValue,
			timeInJit,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.StandardJitFailures,
			prometheus.GaugeValue,
			process.StandardJitFailures,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalNumberofILBytesJitted,
			prometheus.CounterValue,
			process.TotalNumberofILBytesJitted,
			name,
		)
	}

	return nil, nil
}

type netclrLoading struct {
	Name string

	BytesinLoaderHeap         float64 `perflib:"Bytes in Loader Heap"`
	Currentappdomains         float64 `perflib:"Current appdomains"`
	CurrentAssemblies         float64 `perflib:"Current Assemblies"`
	CurrentClassesLoaded      float64 `perflib:"Current Classes Loaded"`
	TotalAppdomains           float64 `perflib:"Total Appdomains"`
	Totalappdomainsunloaded   float64 `perflib:"Total appdomains unloaded"`
	TotalAssemblies           float64 `perflib:"Total Assemblies"`
	TotalClassesLoaded        float64 `perflib:"Total Classes Loaded"`
	TotalNumberofLoadFailures float64 `perflib:"Total # of Load Failures"`
}

func (c *netclrCollector) collectLoading(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrLoading

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("loading")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.BytesinLoaderHeap,
			prometheus.GaugeValue,
			process.BytesinLoaderHeap,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Currentappdomains,
			prometheus.GaugeValue,
			process.Currentappdomains,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.CurrentAssemblies,
			prometheus.GaugeValue,
			process.CurrentAssemblies,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.CurrentClassesLoaded,
			prometheus.GaugeValue,
			process.CurrentClassesLoaded,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalAppdomains,
			prometheus.CounterValue,
			process.TotalAppdomains,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Totalappdomainsunloaded,
			prometheus.CounterValue,
			process.Totalappdomainsunloaded,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalAssemblies,
			prometheus.CounterValue,
			process.TotalAssemblies,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalClassesLoaded,
			prometheus.CounterValue,
			process.TotalClassesLoaded,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalNumberofLoadFailures,
			prometheus.CounterValue,
			process.TotalNumberofLoadFailures,
			name,
		)
	}

	return nil, nil
}

type netclrLocksAndThreads struct {
	Name string

	CurrentQueueLength               float64 `perflib:"Current Queue Length"`
	NumberofcurrentlogicalThreads    float64 `perflib:"# of current logical Threads"`
	NumberofcurrentphysicalThreads   float64 `perflib:"# of current physical Threads"`
	Numberofcurrentrecognizedthreads float64 `perflib:"# of current recognized threads"`
	Numberoftotalrecognizedthreads   float64 `perflib:"# of total recognized threads"`
	QueueLengthPeak                  float64 `perflib:"Queue Length Peak"`
	TotalNumberofContentions         float64 `perflib:"Total # of Contentions"`
}

func (c *netclrCollector) collectLocksAndThreads(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrLocksAndThreads

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("locksandthreads")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.CurrentQueueLength,
			prometheus.GaugeValue,
			process.CurrentQueueLength,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofcurrentlogicalThreads,
			prometheus.GaugeValue,
			process.NumberofcurrentlogicalThreads,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofcurrentphysicalThreads,
			prometheus.GaugeValue,
			process.NumberofcurrentphysicalThreads,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Numberofcurrentrecognizedthreads,
			prometheus.GaugeValue,
			process.Numberofcurrentrecognizedthreads,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Numberoftotalrecognizedthreads,
			prometheus.CounterValue,
			process.Numberoftotalrecognizedthreads,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.QueueLengthPeak,
			prometheus.CounterValue,
			process.QueueLengthPeak,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalNumberofContentions,
			prometheus.CounterValue,
			process.TotalNumberofContentions,
			name,
		)
	}

	return nil, nil
}

type netclrMemory struct {
	Name string

	AllocatedBytesPersec               float64 `perflib:"Allocated Bytes/sec"`
	FinalizationSurvivors              float64 `perflib:"Finalization Survivors"`
	Frequency_PerfTime                 float64 `perflib:"Not Displayed_Base"`
	Gen0heapsize                       float64 `perflib:"Gen 0 heap size"`
	Gen0PromotedBytesPerSec            float64 `perflib:"Gen 0 Promoted Bytes/Sec"`
	Gen1heapsize                       float64 `perflib:"Gen 1 heap size"`
	Gen1PromotedBytesPerSec            float64 `perflib:"Gen 1 Promoted Bytes/Sec"`
	Gen2heapsize                       float64 `perflib:"Gen 2 heap size"`
	LargeObjectHeapsize                float64 `perflib:"Large Object Heap size"`
	NumberGCHandles                    float64 `perflib:"# GC Handles"`
	NumberGen0Collections              float64 `perflib:"# Gen 0 Collections"`
	NumberGen1Collections              float64 `perflib:"# Gen 1 Collections"`
	NumberGen2Collections              float64 `perflib:"# Gen 2 Collections"`
	NumberInducedGC                    float64 `perflib:"# Induced GC"`
	NumberofPinnedObjects              float64 `perflib:"# of Pinned Objects"`
	NumberofSinkBlocksinuse            float64 `perflib:"# of Sink Blocks in use"`
	NumberTotalcommittedBytes          float64 `perflib:"# Total committed Bytes"`
	NumberTotalreservedBytes           float64 `perflib:"# Total reserved Bytes"`
	PercentTimeinGC                    float64 `perflib:"% Time in GC"`
	ProcessID                          float64 `perflib:"Process ID"`
	PromotedFinalizationMemoryfromGen0 float64 `perflib:"Promoted Finalization-Memory from Gen 0"`
	PromotedMemoryfromGen0             float64 `perflib:"Promoted Memory from Gen 0"`
	PromotedMemoryfromGen1             float64 `perflib:"Promoted Memory from Gen 1"`
}

func (c *netclrCollector) collectMemory(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrMemory

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("memory")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.AllocatedBytes,
			prometheus.CounterValue,
			process.AllocatedBytesPersec,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.FinalizationSurvivors,
			prometheus.GaugeValue,
			process.FinalizationSurvivors,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.HeapSize,
			prometheus.GaugeValue,
			process.Gen0heapsize,
			name,
			"Gen0",
		)

		ch <- prometheus.MustNewConstMetric(
			c.PromotedBytes,
			prometheus.GaugeValue,
			process.Gen0PromotedBytesPerSec,
			name,
			"Gen0",
		)

		ch <- prometheus.MustNewConstMetric(
			c.HeapSize,
			prometheus.GaugeValue,
			process.Gen1heapsize,
			name,
			"Gen1",
		)

		ch <- prometheus.MustNewConstMetric(
			c.PromotedBytes,
			prometheus.GaugeValue,
			process.Gen1PromotedBytesPerSec,
			name,
			"Gen1",
		)

		ch <- prometheus.MustNewConstMetric(
			c.HeapSize,
			prometheus.GaugeValue,
			process.Gen2heapsize,
			name,
			"Gen2",
		)

		ch <- prometheus.MustNewConstMetric(
			c.HeapSize,
			prometheus.GaugeValue,
			process.LargeObjectHeapsize,
			name,
			"LOH",
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberGCHandles,
			prometheus.GaugeValue,
			process.NumberGCHandles,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberCollections,
			prometheus.CounterValue,
			process.NumberGen0Collections,
			name,
			"Gen0",
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberCollections,
			prometheus.CounterValue,
			process.NumberGen1Collections,
			name,
			"Gen1",
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberCollections,
			prometheus.CounterValue,
			process.NumberGen2Collections,
			name,
			"Gen2",
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberInducedGC,
			prometheus.CounterValue,
			process.NumberInducedGC,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofPinnedObjects,
			prometheus.GaugeValue,
			process.NumberofPinnedObjects,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberofSinkBlocksinuse,
			prometheus.GaugeValue,
			process.NumberofSinkBlocksinuse,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberTotalCommittedBytes,
			prometheus.GaugeValue,
			process.NumberTotalcommittedBytes,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.NumberTotalreservedBytes,
			prometheus.GaugeValue,
			process.NumberTotalreservedBytes,
			name,
		)

		timeinGC := 0.0
		if process.Frequency_PerfTime != 0 {
			timeinGC = process.PercentTimeinGC / process.Frequency_PerfTime
		}
		ch <- prometheus.MustNewConstMetric(
			c.TimeinGC,
			prometheus.GaugeValue,
			timeinGC,
			name,
		)
	}

	return nil, nil
}

type netclrRemoting struct {
	Name string

	Channels                       float64 `perflib:"Channels"`
	ContextBoundClassesLoaded      float64 `perflib:"Context-Bound Classes Loaded"`
	ContextBoundObjectsAllocPersec float64 `perflib:"Context-Bound Objects Alloc / sec"`
	ContextProxies                 float64 `perflib:"Context Proxies"`
	Contexts                       float64 `perflib:"Contexts"`
	RemoteCallsPersec              float64 `perflib:"Remote Calls/sec"`
	TotalRemoteCalls               float64 `perflib:"Total Remote Calls"`
}

func (c *netclrCollector) collectRemoting(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrRemoting

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("remoting")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.Channels,
			prometheus.CounterValue,
			process.Channels,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.ContextBoundClassesLoaded,
			prometheus.GaugeValue,
			process.ContextBoundClassesLoaded,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.ContextBoundObjects,
			prometheus.CounterValue,
			process.ContextBoundObjectsAllocPersec,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.ContextProxies,
			prometheus.CounterValue,
			process.ContextProxies,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.Contexts,
			prometheus.GaugeValue,
			process.Contexts,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalRemoteCalls,
			prometheus.CounterValue,
			process.TotalRemoteCalls,
			name,
		)
	}

	return nil, nil
}

type netclrSecurity struct {
	Name string

	Frequency_PerfTime    float64 `perflib:"Not Displayed_Base"`
	NumberLinkTimeChecks  float64 `perflib:"# Link Time Checks"`
	PercentTimeinRTchecks float64 `perflib:"% Time in RT checks"`
	StackWalkDepth        float64 `perflib:"Stack Walk Depth"`
	TotalRuntimeChecks    float64 `perflib:"Total Runtime Checks"`
}

func (c *netclrCollector) collectSecurity(ctx *ScrapeContext, ch chan<- prometheus.Metric) (*prometheus.Desc, error) {
	var dst []netclrSecurity

	perfObject := ctx.perfObjects[netclrGetPerfObjectName("security")]
	if err := unmarshalObject(perfObject, &dst); err != nil {
		return nil, err
	}

	names := make(map[string]int, len(dst))
	for _, process := range dst {
		name, keep := c.netclrMapProcessName(process.Name, names)
		if !keep {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			c.NumberLinkTimeChecks,
			prometheus.CounterValue,
			process.NumberLinkTimeChecks,
			name,
		)

		timeinRTchecks := 0.0
		if process.Frequency_PerfTime != 0 {
			timeinRTchecks = process.PercentTimeinRTchecks / process.Frequency_PerfTime
		}
		ch <- prometheus.MustNewConstMetric(
			c.TimeinRTchecks,
			prometheus.GaugeValue,
			timeinRTchecks,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.StackWalkDepth,
			prometheus.GaugeValue,
			process.StackWalkDepth,
			name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.TotalRuntimeChecks,
			prometheus.CounterValue,
			process.TotalRuntimeChecks,
			name,
		)
	}

	return nil, nil
}
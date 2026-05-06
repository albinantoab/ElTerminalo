//go:build darwin

package stats

/*
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <sys/sysctl.h>

// All Mach reads go through tiny C wrappers so cgo doesn't have to wrestle
// with Apple's macros and union typedefs.

static int read_cpu_ticks(uint64_t *user, uint64_t *sys, uint64_t *nice, uint64_t *idle) {
    host_cpu_load_info_data_t info;
    mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
    if (host_statistics(mach_host_self(), HOST_CPU_LOAD_INFO,
                        (host_info_t)&info, &count) != KERN_SUCCESS) {
        return -1;
    }
    *user = (uint64_t)info.cpu_ticks[CPU_STATE_USER];
    *sys  = (uint64_t)info.cpu_ticks[CPU_STATE_SYSTEM];
    *nice = (uint64_t)info.cpu_ticks[CPU_STATE_NICE];
    *idle = (uint64_t)info.cpu_ticks[CPU_STATE_IDLE];
    return 0;
}

static int read_vm_stats(uint64_t *free_p, uint64_t *speculative_p,
                          uint64_t *external_p, uint64_t *page_size) {
    vm_statistics64_data_t info;
    mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
    if (host_statistics64(mach_host_self(), HOST_VM_INFO64,
                          (host_info64_t)&info, &count) != KERN_SUCCESS) {
        return -1;
    }
    vm_size_t ps = 0;
    host_page_size(mach_host_self(), &ps);
    *page_size = (uint64_t)ps;
    *free_p = (uint64_t)info.free_count;
    *speculative_p = (uint64_t)info.speculative_count;
    *external_p = (uint64_t)info.external_page_count;
    return 0;
}

static uint64_t read_total_ram(void) {
    uint64_t mem = 0;
    size_t len = sizeof(mem);
    if (sysctlbyname("hw.memsize", &mem, &len, NULL, 0) != 0) return 0;
    return mem;
}
*/
import "C"

// readCPUTicks returns aggregate host CPU ticks: busy = user+sys+nice,
// total = busy+idle. Values are monotonically increasing since boot, so the
// caller takes deltas between successive samples to compute %.
func readCPUTicks() (busy, total uint64, ok bool) {
	var user, sys, nice, idle C.uint64_t
	if C.read_cpu_ticks(&user, &sys, &nice, &idle) != 0 {
		return 0, 0, false
	}
	busy = uint64(user) + uint64(sys) + uint64(nice)
	total = busy + uint64(idle)
	return busy, total, true
}

// readMemory returns currently used and total host memory in bytes.
//
// "Used" matches Activity Monitor's "Memory Used": total minus the pages the
// kernel can reclaim without paging — free, speculative, and file-backed
// (external) pages. File-backed pages are what Activity Monitor calls
// "Cached Files" and explicitly excludes from Memory Used.
func readMemory() (used, total uint64) {
	totalRAM := uint64(C.read_total_ram())
	if totalRAM == 0 {
		return 0, 0
	}

	var free, speculative, external, pageSize C.uint64_t
	if C.read_vm_stats(&free, &speculative, &external, &pageSize) != 0 {
		return 0, totalRAM
	}

	ps := uint64(pageSize)
	reclaimable := (uint64(free) + uint64(speculative) + uint64(external)) * ps
	available := min(reclaimable, totalRAM)
	return totalRAM - available, totalRAM
}

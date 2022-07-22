package util

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type PodStartupLatencyTracker struct {
	pods map[types.UID]*perPodState
}

type perPodState struct {
	creationTimestamp time.Time
	
	firstStartedPulling time.Time
	lastFinishedPulling time.Time

	firstResourceVersionWhenPodWasReportedAsRunning string
	recorded bool
}

func NewPodStartupLatencyTracker() *PodStartupLatencyTracker {
	return &PodStartupLatencyTracker{
		pods: make(map[types.UID]*perPodState),
	}
}

// ObservedPodOnWatch to be called from somewhere where we look for pods.
func (p *PodStartupLatencyTracker) ObservedPodOnWatch(pod *v1.Pod, when time.Time) {
	state := p.pods[podUID]
	if state == nil {
		return // create?
	}
	if state.recorded {
		// Already recorded latency for this pod.
		return
	}
	if state.firstResourceVersionWhenPodWasReportedAsRunning == pod.ResourceVersion { // Should we cast to int64 and compare? Probably...
		imagePullingDuration = state.lastFinishedPulling.Sub(state.firstStartedPulling)
		metrics.PodStartuplatency.Observe(when.Sub(state.creationTimestamp) - imagePullingDuration)
		state.recorded = true
	}
}

func (p *PodStartupLatencyTracker) RecordImageStartedPulling(podUID types.UID) {
	state := p.pods[podUID]
	if state == nil {
		return
	}
	if state.firstStartedPulling == time.Time{} { // not set
		state.firstStartedPulling = time.Now()
	}
}
func (p *PodStartupLatencyTracker) RecordImageFinishedPulling(podUID types.UID) {
	state := p.pods[podUID]
	if state == nil {
		return
	}
	state.lastFinishedPulling = time.Now() // Now is always grater than values from the past.
}

func (p *PodStartupLatencyTracker) RecordStatusUpdated(pod *v1.Pod) {
	podUID := pod.UID
	state := p.pods[podUID]
	if state == nil {
		return
	}
	if state.firstResourceVersionWhenPodWasReportedAsRunning != "" {
		// Already started.
		return
	}
	if PodStatusRunning(pod.Status) {
		// It's first time we see pod startup running. Let's record pod.ResourceVersion
		state.firstResourceVersionWhenPodWasReportedAsRunning = pod.ResourceVersion
	}
}

func PodStatusRunning(*v1.PodStatus) bool {
	// TODO
}

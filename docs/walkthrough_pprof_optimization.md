# Walkthrough - Optimize alloc_space in kube-controller-manager

I have implemented two major optimizations to reduce memory allocations (`alloc_space`) in `kube-controller-manager`, targeting the top allocators identified in the pprof profile.

## Optimization 1: Toleration Helpers and DaemonSet Controller

### Changes Made

#### Core V1 Helpers
- **[helpers.go](file:///usr/local/google/home/maciejborsz/k8s.io/kubernetes/pkg/apis/core/v1/helper/helpers.go)**
    - Optimized `AddOrUpdateTolerationInPodSpec` to avoid allocations when the toleration is already present and identical.
    - Fixed a bug where `helper.Semantic.DeepEqual` was comparing a pointer to a value, which likely always returned false and caused unnecessary allocations.
    - Added `AddOrUpdateTolerationsInPodSpec` to allow batching multiple toleration updates, reducing allocations further.

#### DaemonSet Controller
- **[daemonset_util.go](file:///usr/local/google/home/maciejborsz/k8s.io/kubernetes/pkg/controller/daemon/util/daemonset_util.go)**
    - Updated `AddOrUpdateDaemonPodTolerations` to use the new batched API `AddOrUpdateTolerationsInPodSpec`, replacing 6-7 individual calls with a single call.

### Verification Results
- Added unit tests in [helpers_test.go](file:///usr/local/google/home/maciejborsz/k8s.io/kubernetes/pkg/apis/core/v1/helper/helpers_test.go).
- Ran tests successfully.
- **Benchmarks**:
    ```
    BenchmarkAddOrUpdateTolerationInPodSpec_Sequential-64             970117              1430 ns/op            1568 B/op          6 allocs/op
    BenchmarkAddOrUpdateTolerationsInPodSpec_Batched-64              2749539               411.4 ns/op            448 B/op          1 allocs/op
    ```
    - The batched version is about **3.5x faster**, uses **3.5x less memory**, and makes **6x fewer allocations**.

---

## Optimization 2: NodeShouldRunDaemonPod in DaemonSet Controller

### Changes Made

#### DaemonSet Controller
- **[daemon_controller.go](file:///usr/local/google/home/maciejborsz/k8s.io/kubernetes/pkg/controller/daemon/daemon_controller.go)**
    - Refactor `NodeShouldRunDaemonPod` to avoid calling `NewPod`, which was allocating a lot of memory by copying the full `PodSpec`.
    - Used a minimal `Pod` instance only for the `nodeaffinity.GetRequiredNodeAffinity` call to satisfy its API without full copy.
    - Implemented custom non-allocating taint matching logic (`ToleratesTaints` and helpers) to check if a node's taints are tolerated by the DaemonSet pod template and default tolerations.

### Verification Results
- Ran unit tests in `pkg/controller/daemon` successfully:
    ```
    ok      k8s.io/kubernetes/pkg/controller/daemon 1.010s
    ```
- **Benchmarks**: Ran benchmarks comparing the new optimized `NodeShouldRunDaemonPod` with the old style (creating a pod):
    ```
    BenchmarkNodeShouldRunDaemonPod_New-64           5666220               207.3 ns/op           128 B/op          4 allocs/op
    BenchmarkNodeShouldRunDaemonPod_OldStyle-64      1000000              1002 ns/op            1856 B/op          6 allocs/op
    ```
    - The new version is about **5x faster**.
    - Uses about **14.5x less memory** (128 B vs 1856 B).
    - Makes fewer allocations (4 vs 6).

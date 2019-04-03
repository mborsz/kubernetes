#!/bin/bash -x
TMP=$(mktemp -p ~/tmp -d)

echo "USING ${TMP}"

export KUBECONFIG=$TMP/kubeconfig
export KUBE_GCE_NETWORK=e2e-maciejborsz
export KUBE_GCE_INSTANCE_PREFIX=e2e-maciejborsz

export INSTANCE_PREFIX=e2e-maciejborsz
export KUBE_GCS_UPDATE_LATEST=n
export KUBE_FASTBUILD=true
export KUBE_GCE_ENABLE_IP_ALIASES=true
export CREATE_CUSTOM_NETWORK=true
export ENABLE_HOLLOW_NODE_LOGS=true
export ETCD_TEST_ARGS="--enable-pprof"
export CONTROLLER_MANAGER_TEST_ARGS="--profiling"
export SCHEDULER_TEST_ARGS="--profiling"
export TEST_CLUSTER_LOG_LEVEL="--v=2"
export API_SERVER_TEST_LOG_LEVEL="--v=3"
export TEST_CLUSTER_RESYNC_PERIOD="--min-resync-period=12h"
export KUBEMARK_ETCD_COMPACTION_INTERVAL_SEC=150
export KUBE_FEATURE_GATES="TaintBasedEvictions=true"
export ALLOWED_NOTREADY_NODES=1
export ENABLE_PROMETHEUS_SERVER=false
export KUBEMARK_MASTER_COMPONENTS_QPS_LIMITS="--kube-api-qps=100 --kube-api-burst=100"
export HOLLOW_PROXY_TEST_ARGS=--use-real-proxier=false
export USE_REAL_PROXIER=false

export PROJECT_ID=k8s-scale-testing
export ZONE=us-east1-b
 


go run hack/e2e.go -- \
    --gcp-project=$PROJECT_ID \
    --gcp-zone=$ZONE \
    --check-leaked-resources \
    --gcp-master-size=n1-standard-8 \
    --gcp-node-image=gci \
    --gcp-node-size=n1-standard-8 \
    --gcp-nodes=83 \
    --provider=gce \
    --up \
    --timeout=1080m \
    --kubemark \
    --kubemark-nodes=5000 \
    --test=false \
    --test-cmd=$GOPATH/src/k8s.io/perf-tests/run-e2e.sh \
    --test-cmd-args=cluster-loader2 \
    --test-cmd-args=--nodes=5000 \
    --test-cmd-args=--provider=kubemark \
    --test-cmd-args=--report-dir=/tmp/cluster_loader2/_artifacts \
    --test-cmd-args=--testconfig=testing/load/config.yaml \
    --test-cmd-args="--enable-prometheus-server=false" \
    --test-cmd-name=ClusterLoaderV2 2>&1 | tee $TMP/out.log
# Generic variables.
INSTANCE_PREFIX="e2e-maciejborsz"
SERVICE_CLUSTER_IP_RANGE="10.0.0.0/16"
EVENT_PD="false"

# Etcd related variables.
ETCD_IMAGE="3.3.10-0"
ETCD_VERSION=""

# Controller-manager related variables.
CONTROLLER_MANAGER_TEST_ARGS=" --v=2 --min-resync-period=12h  --kube-api-qps=100 --kube-api-burst=100"
ALLOCATE_NODE_CIDRS="true"
CLUSTER_IP_RANGE="10.64.0.0/11"
TERMINATED_POD_GC_THRESHOLD="100"

# Scheduler related variables.
SCHEDULER_TEST_ARGS=" --v=2  --kube-api-qps=100 --kube-api-burst=100"

# Apiserver related variables.
APISERVER_TEST_ARGS=" --runtime-config=extensions/v1beta1,scheduling.k8s.io/v1alpha1 --v=3  --delete-collection-workers=16"
STORAGE_MEDIA_TYPE=""
STORAGE_BACKEND="etcd3"
ETCD_SERVERS=""
ETCD_SERVERS_OVERRIDES=""
ETCD_COMPACTION_INTERVAL_SEC="150"
RUNTIME_CONFIG="batch/v2alpha1=true"
NUM_NODES="5000"
CUSTOM_ADMISSION_PLUGINS="NamespaceLifecycle,LimitRanger,ServiceAccount,PersistentVolumeLabel,DefaultStorageClass,DefaultTolerationSeconds,NodeRestriction,Priority,StorageObjectInUseProtection,MutatingAdmissionWebhook,ValidatingAdmissionWebhook,ResourceQuota"
FEATURE_GATES="TaintBasedEvictions=true"
KUBE_APISERVER_REQUEST_TIMEOUT="300"
ENABLE_APISERVER_ADVANCED_AUDIT="false"

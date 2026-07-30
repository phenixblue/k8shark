package server

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
)

// defaultObject applies Kubernetes API defaulting to body — e.g. an empty
// Deployment.spec.strategy.type becomes "RollingUpdate" — matching what a real
// apiserver does on every write. The overlay has no apiserver behind it, so
// without this a freshly created object keeps whatever zero values the client
// didn't set; a real controller (e.g. kube-controller-manager's deployment
// controller) assumes defaulting already happened and errors on the zero value
// ("unexpected deployment strategy type: \"\"") instead of treating it as
// "use the default". Resources outside client-go's built-in scheme (CRDs) are
// returned unchanged — there's no way to know their defaults without the CRD's
// schema, which the overlay doesn't have.
//
// scheme.Scheme.Default only runs defaulters registered on the client-side
// scheme, which — for the built-in types this project vendors
// (k8s.io/api, not the full k8s.io/kubernetes apiserver) — is effectively
// none of the fields real controllers care about; the actual
// SetDefaults_Deployment-style functions live in k8s.io/kubernetes's internal
// packages, which aren't meant to be imported as a library. applyKnownDefaults
// hand-covers the specific, long-stable defaults our curated
// --with-controller-manager controllers rely on instead.
func defaultObject(gvk schema.GroupVersionKind, body json.RawMessage) json.RawMessage {
	typed, err := scheme.Scheme.New(gvk)
	if err != nil {
		return body
	}
	if err := json.Unmarshal(body, typed); err != nil {
		return body
	}
	// Marshal the client's own object back out before defaulting (rather than
	// reusing body directly) so the merge patch computed below only reflects
	// fields defaulting actually changed, not differences between body's
	// exact bytes/field order and typed's. Round-tripping through the typed
	// struct at all would normally risk silently dropping fields the
	// vendored k8s.io/api types don't know about (e.g. a newer API field) or
	// explicitly-sent zero values omitempty would elide — but since this
	// undefaulted marshal is only ever diffed against the defaulted one, not
	// returned, neither loss ends up in the result: a field defaulting
	// doesn't touch is absent from the diff either way, and the merge patch
	// below is applied onto the original body, preserving every field body
	// actually had.
	before, err := json.Marshal(typed)
	if err != nil {
		return body
	}
	scheme.Scheme.Default(typed)
	applyKnownDefaults(typed)
	after, err := json.Marshal(typed)
	if err != nil {
		return body
	}
	patch, err := jsonpatch.CreateMergePatch(before, after)
	if err != nil {
		return body
	}
	defaulted, err := jsonpatch.MergePatch(body, patch)
	if err != nil {
		return body
	}
	return defaulted
}

// applyKnownDefaults hand-applies the handful of long-stable Kubernetes API
// defaults that the controllers --with-controller-manager enables (see
// cmd/controllermanager.go) assume are already in place: a zero-valued
// strategy/update-strategy/concurrency-policy reads as "invalid", not "use the
// default", to those controllers. Defaulting a *Type without also defaulting
// its matching *RollingUpdate sub-struct is not enough — real
// kube-controller-manager code unconditionally dereferences that pointer
// (e.g. deployment_util.go's NewRSNewReplicas reads
// Spec.Strategy.RollingUpdate.MaxSurge), so a Type of "RollingUpdate" with a
// nil RollingUpdate struct panics deep inside the controller instead of
// erroring cleanly. The same function also dereferences
// *deployment.Spec.Replicas unconditionally, so a manifest that omits
// `replicas` (relying on the apiserver's default of 1, as e.g. Istio's charts
// do) panics the same way — hence Deployment/StatefulSet/ReplicaSet.Replicas
// are defaulted here too.

// isIPv6Address reports whether s parses as an IPv6 address. Used to infer a
// Service's address family from an explicit ClusterIP/ClusterIPs value
// rather than always assuming IPv4; returns false for "" and "None" (a
// headless Service), which correctly fall through to the IPv4 default.
func isIPv6Address(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() == nil
}

// syntheticLoadBalancerIP deterministically derives a fake external address
// for a LoadBalancer Service from its identity, landing in TEST-NET-3
// (203.0.113.0/24 — reserved by RFC 5737 for documentation/example use, so it
// can never collide with anything real). Deterministic rather than a counter
// so the same Service gets the same address across repeated writes instead of
// a new one every time defaultObject runs.
func syntheticLoadBalancerIP(namespace, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespace + "/" + name))
	return fmt.Sprintf("203.0.113.%d", 1+h.Sum32()%254)
}

// syntheticClusterIP deterministically derives a fake ClusterIP for a Service
// from its identity, landing in 10.96.0.0/12 — the conventional kubeadm
// service-cluster-ip-range, so it looks at home next to a typical capture's
// real ClusterIPs. Deterministic (like syntheticLoadBalancerIP) rather than a
// counter, so the same Service keeps the same address across repeated writes.
func syntheticClusterIP(namespace, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte("clusterip/" + namespace + "/" + name))
	sum := h.Sum32()
	// 10.96.0.0/12 spans 10.96.0.0-10.111.255.255: only the low 4 bits of the
	// second octet vary (96 = 0110_0000 through 111 = 0110_1111).
	return fmt.Sprintf("10.%d.%d.%d", 96+sum%16, (sum>>8)%256, 1+(sum>>16)%254)
}

func applyKnownDefaults(obj runtime.Object) {
	switch o := obj.(type) {
	case *corev1.Service:
		if o.Spec.Type == "" {
			o.Spec.Type = corev1.ServiceTypeClusterIP
		}
		if o.Spec.SessionAffinity == "" {
			o.Spec.SessionAffinity = corev1.ServiceAffinityNone
		}
		// The real apiserver's IP allocator assigns every Service a ClusterIP —
		// even LoadBalancer/NodePort ones — as soon as it's created; the overlay
		// has no IPAM, so nothing else ever populates this. kstatus's readiness
		// check for a LoadBalancer Service (which Helm v4's `--wait` uses via
		// sigs.k8s.io/cli-utils) specifically requires spec.clusterIP to be
		// non-empty — NOT status.loadBalancer.ingress — so a Service that never
		// gets one hangs `helm install --wait` forever regardless of the
		// synthesized external address below. A headless Service
		// (clusterIP: "None", set explicitly by the client) and ExternalName
		// Services are left alone — they don't get one on a real cluster either.
		if o.Spec.Type != corev1.ServiceTypeExternalName && o.Spec.ClusterIP == "" {
			ip := syntheticClusterIP(o.Namespace, o.Name)
			o.Spec.ClusterIP = ip
			if len(o.Spec.ClusterIPs) == 0 {
				o.Spec.ClusterIPs = []string{ip}
			}
		}
		if len(o.Spec.IPFamilies) == 0 {
			// The endpoint/endpointslice controllers index IPFamilies[0]
			// unconditionally; a real apiserver always populates this from the
			// cluster's configured service-cluster-ip-range. Infer the primary
			// family from the primary address only — ClusterIP if set, else
			// ClusterIPs[0] — never by scanning every ClusterIPs entry: for a
			// dual-stack Service with an IPv4 ClusterIP and an IPv6 secondary in
			// ClusterIPs, scanning all entries would pick IPv6 and produce
			// IPFamilies[0] inconsistent with the primary ClusterIP, sending the
			// endpoint controller down the wrong address family. IPv4 remains the
			// fallback when nothing indicates IPv6, matching every capture this
			// project has captured so far.
			family := corev1.IPv4Protocol
			switch {
			case o.Spec.ClusterIP != "" && o.Spec.ClusterIP != corev1.ClusterIPNone:
				if isIPv6Address(o.Spec.ClusterIP) {
					family = corev1.IPv6Protocol
				}
			case len(o.Spec.ClusterIPs) > 0:
				if isIPv6Address(o.Spec.ClusterIPs[0]) {
					family = corev1.IPv6Protocol
				}
			}
			o.Spec.IPFamilies = []corev1.IPFamily{family}
		}
		// A real cloud-controller-manager (or an on-prem equivalent like MetalLB)
		// eventually assigns a LoadBalancer Service an external address;
		// --with-controller-manager's curated set deliberately excludes
		// cloud-provider controllers (see cmd/controllermanager.go — no real cloud
		// provider to ask), so nothing else in the overlay ever populates this
		// field. Without it, `helm install --wait` and any other client polling
		// for a LoadBalancer Service to become ready hangs forever. Synthesize an
		// address deterministically from the Service's identity (rather than a
		// counter) so repeated writes to the same Service don't reassign it.
		if o.Spec.Type == corev1.ServiceTypeLoadBalancer && len(o.Status.LoadBalancer.Ingress) == 0 {
			o.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
				{IP: syntheticLoadBalancerIP(o.Namespace, o.Name)},
			}
		}
	case *appsv1.Deployment:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.Strategy.Type == "" {
			o.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		}
		if o.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && o.Spec.Strategy.RollingUpdate == nil {
			maxUnavailable := intstr.FromString("25%")
			maxSurge := intstr.FromString("25%")
			o.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			}
		}
	case *appsv1.ReplicaSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
	case *appsv1.DaemonSet:
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateDaemonSetStrategyType
		}
		if o.Spec.UpdateStrategy.Type == appsv1.RollingUpdateDaemonSetStrategyType && o.Spec.UpdateStrategy.RollingUpdate == nil {
			maxUnavailable := intstr.FromString("25%")
			maxSurge := intstr.FromInt32(0)
			o.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			}
		}
		// pkg/controller/daemon/update.go's cleanupHistory dereferences
		// *ds.Spec.RevisionHistoryLimit unconditionally (`toKeep :=
		// int(*ds.Spec.RevisionHistoryLimit)`) — unlike the deployment
		// controller's equivalent cleanup, which nil-checks first
		// (HasRevisionHistoryLimit). A nil value here crashes the whole
		// daemonset controller goroutine (client-go's crash handler logs it,
		// then repanics — this isn't a swallowed panic like the others in this
		// function, it takes the controller-manager process down).
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
	case *appsv1.StatefulSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateStatefulSetStrategyType
		}
		// pkg/controller/statefulset/stateful_set_control.go's truncateHistory
		// dereferences *set.Spec.RevisionHistoryLimit unconditionally
		// (`historyLimit := int(*set.Spec.RevisionHistoryLimit)`) — same
		// unguarded-pointer class as DaemonSet's cleanupHistory above, and
		// likewise fatal to the whole controller-manager process, not just
		// that one StatefulSet's reconcile.
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		if o.Spec.PodManagementPolicy == "" {
			o.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
		}
		if o.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && o.Spec.UpdateStrategy.RollingUpdate == nil {
			partition := int32(0)
			o.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition}
		}
	case *batchv1.Job:
		if o.Spec.Parallelism == nil {
			o.Spec.Parallelism = ptr.To(int32(1))
		}
		if o.Spec.BackoffLimit == nil {
			o.Spec.BackoffLimit = ptr.To(int32(6))
		}
		if o.Spec.CompletionMode == nil {
			o.Spec.CompletionMode = ptr.To(batchv1.NonIndexedCompletion)
		}
		if o.Spec.Suspend == nil {
			o.Spec.Suspend = ptr.To(false)
		}
		if o.Spec.ManualSelector == nil {
			o.Spec.ManualSelector = ptr.To(false)
		}
		if o.Spec.PodReplacementPolicy == nil {
			o.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}
	case *batchv1.CronJob:
		if o.Spec.ConcurrencyPolicy == "" {
			o.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
		}
		if o.Spec.Suspend == nil {
			o.Spec.Suspend = ptr.To(false)
		}
		if o.Spec.SuccessfulJobsHistoryLimit == nil {
			o.Spec.SuccessfulJobsHistoryLimit = ptr.To(int32(3))
		}
		if o.Spec.FailedJobsHistoryLimit == nil {
			o.Spec.FailedJobsHistoryLimit = ptr.To(int32(1))
		}
	}
}

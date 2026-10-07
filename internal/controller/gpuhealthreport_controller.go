package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	healthv1alpha1 "example.com/gpuhealth/api/v1alpha1"
)

const (
	taintKey = "example.com/gpu-unhealthy"

	// Human override: when set to "true" on a Node, the controller leaves it alone.
	pauseAnnotation = "example.com/gpu-health-paused"

	// Extension 2: at most this fraction of nodes may be remediated at once (minimum 1).
	maxUnhealthyFraction = 0.34
	budgetRetryDelay     = 30 * time.Second
)

// GPUHealthReportReconciler reconciles a GPUHealthReport object
type GPUHealthReportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=health.example.com,resources=gpuhealthreports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=health.example.com,resources=gpuhealthreports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

func (r *GPUHealthReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Read the desired state (the report).
	var report healthv1alpha1.GPUHealthReport
	if err := r.Get(ctx, req.NamespacedName, &report); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Read the actual state (the node).
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: report.Spec.NodeName}, &node); err != nil {
		log.Info("node not found for report", "node", report.Spec.NodeName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2a. Human override: if the node is paused, take no action and say so.
	if node.Annotations[pauseAnnotation] == "true" {
		log.Info("node paused by annotation, skipping", "node", node.Name)
		msg := fmt.Sprintf("paused: no action taken on node %s", node.Name)
		return ctrl.Result{}, r.setStatus(ctx, &report, report.Status.Remediated, msg)
	}

	unhealthy := !report.Spec.Healthy

	// 2b. Extension 2: blast-radius control. Only applies when we are about
	// to NEWLY remediate a node (nodes already tainted by us are left alone).
	if unhealthy && !hasTaint(&node) {
		ok, err := r.withinBudget(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			log.Info("remediation blocked by budget", "node", node.Name)
			msg := fmt.Sprintf("blocked: unhealthy-node budget exhausted, will retry (node %s)", node.Name)
			if err := r.setStatus(ctx, &report, false, msg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: budgetRetryDelay}, nil
		}
	}

	// 3. Compute the difference; only write if something must change (idempotency).
	base := node.DeepCopy()
	changed := false

	if node.Spec.Unschedulable != unhealthy {
		node.Spec.Unschedulable = unhealthy
		changed = true
	}
	if ensureTaint(&node, unhealthy) {
		changed = true
	}
	if changed {
		if err := r.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("remediated node", "node", node.Name, "unhealthy", unhealthy)
	}

	// 4. Report the outcome.
	msg := fmt.Sprintf("node %s unschedulable=%v", node.Name, unhealthy)
	return ctrl.Result{}, r.setStatus(ctx, &report, unhealthy, msg)
}

// setStatus writes status only when it differs (avoids pointless update loops).
func (r *GPUHealthReportReconciler) setStatus(ctx context.Context, report *healthv1alpha1.GPUHealthReport, remediated bool, msg string) error {
	if report.Status.Remediated == remediated && report.Status.Message == msg {
		return nil
	}
	report.Status.Remediated = remediated
	report.Status.Message = msg
	return r.Status().Update(ctx, report)
}

// withinBudget reports whether one more node may be remediated.
func (r *GPUHealthReportReconciler) withinBudget(ctx context.Context) (bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return false, err
	}
	tainted := 0
	for i := range nodes.Items {
		if hasTaint(&nodes.Items[i]) {
			tainted++
		}
	}
	allowed := int(float64(len(nodes.Items)) * maxUnhealthyFraction)
	if allowed < 1 {
		allowed = 1
	}
	return tainted < allowed, nil
}

func hasTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == taintKey {
			return true
		}
	}
	return false
}

// ensureTaint adds or removes our taint and reports whether it changed anything.
func ensureTaint(node *corev1.Node, want bool) bool {
	for i, t := range node.Spec.Taints {
		if t.Key == taintKey {
			if want {
				return false
			}
			node.Spec.Taints = append(node.Spec.Taints[:i], node.Spec.Taints[i+1:]...)
			return true
		}
	}
	if want {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
			Key:    taintKey,
			Effect: corev1.TaintEffectNoSchedule,
		})
		return true
	}
	return false
}

// Extension 1: map a Node event to the reports that refer to that node.
func (r *GPUHealthReportReconciler) reportsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	var reports healthv1alpha1.GPUHealthReportList
	if err := r.List(ctx, &reports); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, rep := range reports.Items {
		if rep.Spec.NodeName == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: rep.Name}})
		}
	}
	return reqs
}

// SetupWithManager registers the controller and tells it what to watch.
func (r *GPUHealthReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&healthv1alpha1.GPUHealthReport{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.reportsForNode)).
		Named("gpuhealthreport").
		Complete(r)
}

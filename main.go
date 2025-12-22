// main.go
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	AnnotationKeyOrigin     = "secret-copy/origin"     // added to copied secrets to track origin
	AnnotationKeyNamespaces = "secret-copy/namespaces" // annotation on source secret
	AdminNamespace          = "admin"
	CopiedByLabelKey        = "secret-copy/from" // label to mark copied secrets (for easier listing)
)

type SecretReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func parseAnnotationList(val string) []string {
	val = strings.TrimSpace(val)
	if val == "" {
		return []string{}
	}
	// support comma-separated values, accept spaces
	r := csv.NewReader(strings.NewReader(val))
	r.TrimLeadingSpace = true
	records, err := r.Read()
	if err != nil {
		// fallback: split on comma
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	out := make([]string, 0, len(records))
	for _, p := range records {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	// only reconcile secrets in admin namespace
	if req.Namespace != AdminNamespace {
		return ctrl.Result{}, nil
	}

	var src corev1.Secret
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		if errors.IsNotFound(err) {
			// Source secret deleted — we won't automatically delete copies in this implementation.
			logger.Info("source secret not found (deleted)", "secret", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	annVal, has := src.Annotations[AnnotationKeyNamespaces]
	if !has {
		// annotation not present -> ignore
		logger.Info("annotation missing; ignoring secret", "secret", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// parse annotation
	targetList := parseAnnotationList(annVal)
	copyToAll := false
	if len(targetList) == 0 {
		// empty list -> treat as "copy to all namespaces"
		copyToAll = true
	}

	logger.Info("reconciling secret", "name", src.Name, "copyToAll", copyToAll, "targets", targetList)

	// Build desired set of namespaces
	var namespaces []string
	if copyToAll {
		// list all namespaces
		var nsList corev1.NamespaceList
		if err := r.List(ctx, &nsList); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing namespaces: %w", err)
		}
		for _, ns := range nsList.Items {
			// skip admin namespace (don't copy to source)
			if ns.Name == AdminNamespace {
				continue
			}
			namespaces = append(namespaces, ns.Name)
		}
	} else {
		for _, n := range targetList {
			if n == AdminNamespace {
				// skip copying into admin itself
				continue
			}
			namespaces = append(namespaces, n)
		}
	}

	// Build the canonical origin/label value (no slashes - valid label)
	originLabelVal := fmt.Sprintf("%s-%s", AdminNamespace, src.Name)
	originAnnotationVal := fmt.Sprintf("%s/%s", AdminNamespace, src.Name) // annotations may keep slash if you want

	// Ensure each target has the secret
	for _, ns := range namespaces {
		var existing corev1.Secret
		key := types.NamespacedName{Namespace: ns, Name: src.Name}
		err := r.Get(ctx, key, &existing)
		if errors.IsNotFound(err) {
			// Create a copy
			toCreate := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      src.Name,
					Namespace: ns,
					Labels: map[string]string{
						CopiedByLabelKey: originLabelVal,
					},
					Annotations: map[string]string{
						AnnotationKeyOrigin: originAnnotationVal,
					},
				},
				Type: src.Type,
				Data: src.Data,
			}
			if err := r.Create(ctx, &toCreate); err != nil {
				logger.Error(err, "creating copied secret", "namespace", ns)
				return ctrl.Result{}, err
			}
			logger.Info("created copied secret", "ns", ns, "name", src.Name)
			continue
		} else if err != nil {
			return ctrl.Result{}, err
		}
		// Exists — determine whether an update is required
		needUpdate := false
		if existing.Type != src.Type {
			existing.Type = src.Type
			needUpdate = true
		}
		// compare data (shallow)
		if !equalSecretData(existing.Data, src.Data) {
			existing.Data = src.Data
			needUpdate = true
		}
		// ensure annotation/label exist
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		if existing.Annotations[AnnotationKeyOrigin] != originAnnotationVal {
			existing.Annotations[AnnotationKeyOrigin] = originAnnotationVal
			needUpdate = true
		}
		if existing.Labels[CopiedByLabelKey] != originLabelVal {
			existing.Labels[CopiedByLabelKey] = originLabelVal
			needUpdate = true
		}
		if needUpdate {
			if err := r.Update(ctx, &existing); err != nil {
				logger.Error(err, "updating copied secret", "ns", ns, "name", src.Name)
				return ctrl.Result{}, err
			}
			logger.Info("updated copied secret", "ns", ns, "name", src.Name)
		} else {
			logger.V(1).Info("copied secret already up-to-date", "ns", ns, "name", src.Name)
		}
	}

	// Handle removal from namespaces no longer desired:
	// Find secrets with label secret-copy/from=<originLabelVal> and delete those in namespaces not in desired set.
	var copiedList corev1.SecretList
	if err := r.List(ctx, &copiedList, client.MatchingLabels{CopiedByLabelKey: originLabelVal}); err != nil {
		// If list fails, just return error so it will retry.
		return ctrl.Result{}, fmt.Errorf("listing copied secrets: %w", err)
	}

	desired := map[string]struct{}{}
	for _, x := range namespaces {
		desired[x] = struct{}{}
	}

	for _, cs := range copiedList.Items {
		// skip source admin if present
		if cs.Namespace == AdminNamespace {
			continue
		}
		if _, ok := desired[cs.Namespace]; !ok {
			// delete
			if err := r.Delete(ctx, &cs); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "deleting stale copied secret", "ns", cs.Namespace, "name", cs.Name)
				return ctrl.Result{}, err
			}
			logger.Info("deleted copied secret no longer desired", "ns", cs.Namespace, "name", cs.Name)
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func equalSecretData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if string(va) != string(vb) {
			return false
		}
	}
	return true
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	cfg, err := rest.InClusterConfig()
	insideCluster := true
	if err != nil {
		insideCluster = false
		// fallback to kubeconfig
		cfg, err = ctrl.GetConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to get kubeconfig: %v\n", err)
			os.Exit(1)
		}
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to start manager: %v\n", err)
		os.Exit(1)
	}

	if err := (&SecretReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "unable to create controller: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("starting manager (inCluster=%v)\n", insideCluster)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "problem running manager: %v\n", err)
		os.Exit(1)
	}
}

func (r *SecretReconciler) SetupWithManager(mgr manager.Manager) error {
	// Watch only secrets in admin namespace (predicate)
	pred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetNamespace() == AdminNamespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetNamespace() == AdminNamespace
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object.GetNamespace() == AdminNamespace
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetNamespace() == AdminNamespace
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(pred).
		Complete(r)
}

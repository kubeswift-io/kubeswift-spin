package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

var (
	spinAppGVR = schema.GroupVersionResource{Group: "core.spinkube.dev", Version: "v1alpha1", Resource: "spinapps"}
	sandboxGVR = schema.GroupVersionResource{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxes"}
)

const (
	// createdByLabel marks SpinApps that startupbench created. It is a
	// constant, so it adds no label cardinality anywhere. startupbench only
	// deletes a SpinApp that carries it.
	createdByLabel = "app.kubernetes.io/created-by"
	createdByValue = "startupbench"

	// KubeSwift v0.16.0 pool labels, used only to wait for a warm slot that
	// is old enough (see -min-slot-age).
	poolLabel      = "sandbox.kubeswift.io/pool"
	slotStateLabel = "sandbox.kubeswift.io/slot-state"
)

type config struct {
	namespace, manifest, name, executor, image string
	mode, runPrefix, path, expect, warmPool    string
	runs, directPort, servicePort              int
	interval, timeout, minSlotAge, settle      time.Duration
	startJitter                                time.Duration
	launcherLogs, keep                         bool
}

func runMain(args []string) int {
	var c config
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&c.namespace, "n", "", "namespace to run in (required)")
	fs.StringVar(&c.manifest, "f", "", "SpinApp manifest with one replica (required)")
	fs.StringVar(&c.mode, "mode", "", `"cold" or "warm"; warm runs that do not check out a warm slot are reported and excluded from the summary (required)`)
	fs.StringVar(&c.name, "name", "", "override metadata.name")
	fs.StringVar(&c.executor, "executor", "", "override spec.executor")
	fs.StringVar(&c.image, "image", "", "override spec.image")
	fs.StringVar(&c.warmPool, "warm-pool", "", "SwiftSandboxPool to wait for before each warm run")
	fs.DurationVar(&c.minSlotAge, "min-slot-age", 30*time.Second, "with -warm-pool, wait until a warm slot's pod is this old (KubeSwift v0.16.0 and v0.16.1 can hand out a slot whose guest is not ready yet)")
	fs.IntVar(&c.runs, "runs", 1, "number of runs")
	fs.DurationVar(&c.startJitter, "start-jitter", 0, "wait a uniformly random time below this before each run (use 2s for warm runs: KubeSwift v0.16.0 checks a slot for new work every 2 seconds, and runs started at a fixed offset would always hit the same point of that cycle)")
	fs.StringVar(&c.runPrefix, "run-prefix", "", "prefix for run identifiers (default: the mode)")
	fs.StringVar(&c.path, "path", "/hello", "HTTP path to request")
	fs.StringVar(&c.expect, "expect", "", "substring the response body must contain (default: any non-empty 200 response)")
	fs.IntVar(&c.directPort, "direct-port", 3000, "application port on the pod IP")
	fs.IntVar(&c.servicePort, "service-port", 80, "Service port")
	fs.DurationVar(&c.interval, "interval", 50*time.Millisecond, "HTTP probe interval")
	fs.DurationVar(&c.timeout, "timeout", 3*time.Minute, "per-run limit")
	fs.DurationVar(&c.settle, "settle", time.Second, "keep watching this long after the last required transition")
	fs.BoolVar(&c.launcherLogs, "launcher-logs", false, "also read KubeSwift launcher pod logs for stage timings, warm-pool dispatch and pod watch warnings (needs pods/log; the patterns match KubeSwift v0.16.0 and v0.16.1 output, which is not an API)")
	fs.BoolVar(&c.keep, "keep", false, "keep the SpinApp after a single run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if c.namespace == "" || c.manifest == "" || (c.mode != "cold" && c.mode != "warm") || c.runs < 1 || (c.keep && c.runs != 1) {
		fmt.Fprintln(os.Stderr, "run: -n, -f and -mode cold|warm are required; -runs must be at least 1; -keep needs -runs 1")
		return 2
	}
	if !strings.HasPrefix(c.path, "/") {
		fmt.Fprintln(os.Stderr, "run: -path must start with /")
		return 2
	}
	if c.runPrefix == "" {
		c.runPrefix = c.mode
	}

	app, err := loadSpinApp(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	b, err := newBench(c, app)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := b.cleanup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	for i := 1; i <= c.runs && ctx.Err() == nil; i++ {
		if c.warmPool != "" {
			if err := b.waitWarmSlot(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "run:", err)
				return 1
			}
		}
		if c.startJitter > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(rand.Int64N(int64(c.startJitter)))): //nolint:gosec // timing jitter, not security
			}
		}
		res, err := b.once(ctx, fmt.Sprintf("%s-%d", c.runPrefix, i))
		if err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			return 1
		}
		_ = enc.Encode(res)
		fmt.Fprintf(os.Stderr, "%s: direct %s, service %s, available %s\n", res.Run,
			msText(res.Metrics, "direct_http"), msText(res.Metrics, "service_http"), msText(res.Metrics, "available"))
		if c.keep {
			break
		}
		if err := b.cleanup(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			return 1
		}
	}
	return 0
}

func msText(m map[string]float64, k string) string {
	if v, ok := m[k]; ok {
		return strconv.FormatFloat(v, 'f', 0, 64) + "ms"
	}
	return "-"
}

// loadSpinApp reads the manifest and applies the overrides. startupbench
// follows one sandbox, so the SpinApp must have exactly one replica.
func loadSpinApp(c config) (*unstructured.Unstructured, error) {
	raw, err := os.ReadFile(c.manifest)
	if err != nil {
		return nil, err
	}
	// Through JSON with the unstructured decoder, so integers stay int64.
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.manifest, err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(js); err != nil {
		return nil, fmt.Errorf("%s: %w", c.manifest, err)
	}
	if u.GetAPIVersion() != "core.spinkube.dev/v1alpha1" || u.GetKind() != "SpinApp" {
		return nil, fmt.Errorf("%s: want a core.spinkube.dev/v1alpha1 SpinApp, got %s %s", c.manifest, u.GetAPIVersion(), u.GetKind())
	}
	if r, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); found && r != 1 {
		return nil, fmt.Errorf("%s: spec.replicas is %d; startupbench measures a single replica, set it to 1", c.manifest, r)
	}
	if c.name != "" {
		u.SetName(c.name)
	}
	if u.GetName() == "" {
		return nil, fmt.Errorf("%s: metadata.name is empty", c.manifest)
	}
	for field, v := range map[string]string{"executor": c.executor, "image": c.image} {
		if v != "" {
			if err := unstructured.SetNestedField(u.Object, v, "spec", field); err != nil {
				return nil, err
			}
		}
	}
	u.SetNamespace(c.namespace)
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[createdByLabel] = createdByValue
	u.SetLabels(labels)
	return u, nil
}

type bench struct {
	c      config
	app    *unstructured.Unstructured
	kc     kubernetes.Interface
	dc     dynamic.Interface
	client *http.Client
}

func newBench(c config, app *unstructured.Unstructured) (*bench, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	}
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = "startupbench"
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	// No keep-alive: every probe opens a new connection, as a new client would.
	tr := &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext}
	// Redirects are not followed: only the application itself may answer.
	client := &http.Client{
		Timeout:       time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &bench{c: c, app: app, kc: kc, dc: dc, client: client}, nil
}

func (b *bench) name() string        { return b.app.GetName() }
func (b *bench) sandboxName() string { return b.name() + "-0" }

// recorder keeps the first time each mark was observed.
type recorder struct {
	mu      sync.Mutex
	t0      time.Time
	started bool
	marks   map[string]float64
}

func (r *recorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.t0, r.started = time.Now(), true
}

func (r *recorder) mark(k string) { r.markAt(k, time.Now()) }

func (r *recorder) markAt(k string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || t.Before(r.t0) {
		return
	}
	if _, ok := r.marks[k]; !ok {
		r.marks[k] = float64(t.Sub(r.t0).Microseconds()) / 1000
	}
}

func (r *recorder) has(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.marks[k]
	return ok
}

// target is what the watches learn about the run's sandbox and pod.
type target struct {
	mu         sync.Mutex
	sandboxUID string
	podName    string
	podIP      string
	// pending holds the latest pod per owning SwiftSandbox UID until the
	// sandbox UID is known: the pod event can arrive first (a warm slot's pod
	// is re-parented as soon as the sandbox is created).
	pending map[string]*corev1.Pod
}

func (t *target) pod() (name, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.podName, t.podIP
}

// once creates the SpinApp and records one start. Watches are opened from
// the current resource version before the create request, so no transition
// after Start can be missed.
func (b *bench) once(parent context.Context, run string) (*Result, error) {
	ctx, cancel := context.WithTimeout(parent, b.c.timeout)
	defer cancel()
	wctx, stopWatches := context.WithCancel(parent)
	defer stopWatches()

	rec := &recorder{marks: map[string]float64{}}
	tg := &target{pending: map[string]*corev1.Pod{}}
	ns := b.c.namespace
	if err := b.startWatches(wctx, rec, tg); err != nil {
		return nil, err
	}

	rec.start()
	t0 := rec.t0
	if _, err := b.dc.Resource(spinAppGVR).Namespace(ns).Create(ctx, b.app, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating SpinApp %s/%s: %w", ns, b.name(), err)
	}
	rec.mark("create.returned")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.probeLoop(ctx, rec, "direct", func() string {
			if _, ip := tg.pod(); ip != "" {
				return "http://" + net.JoinHostPort(ip, strconv.Itoa(b.c.directPort)) + b.c.path
			}
			return ""
		})
	}()
	go func() {
		defer wg.Done()
		// Only after the Service exists, so that no negative DNS answer is
		// cached for its name.
		b.probeLoop(ctx, rec, "service", func() string {
			if rec.has("service.added") {
				return fmt.Sprintf("http://%s.%s.svc:%d%s", b.name(), ns, b.c.servicePort, b.c.path)
			}
			return ""
		})
	}()

	for ctx.Err() == nil && (!rec.has(markDirectOK) || !rec.has(markServiceOK) || !rec.has(markAvailable)) {
		time.Sleep(10 * time.Millisecond)
	}
	timedOut := ctx.Err() != nil && parent.Err() == nil
	if !timedOut {
		time.Sleep(b.c.settle)
	}
	cancel()
	wg.Wait()
	stopWatches()

	var lines map[string]int
	if b.c.launcherLogs {
		pod, _ := tg.pod()
		lctx, lcancel := context.WithTimeout(parent, 30*time.Second)
		lines = launcherMarks(lctx, b.kc, ns, pod, rec)
		lcancel()
	}

	rec.mu.Lock()
	marks := make(map[string]float64, len(rec.marks))
	for k, v := range rec.marks {
		marks[k] = v
	}
	rec.mu.Unlock()
	_, checkedOut := marks[markCheckedOut]
	return &Result{
		Run: run, Mode: b.c.mode, SpinApp: b.name(), Start: t0.UTC(), TimedOut: timedOut,
		CheckedOut: checkedOut, Marks: marks, Metrics: computeMetrics(marks), LauncherLines: lines,
	}, nil
}

// probeLoop requests the URL every interval until the expected response
// arrives. url returns "" while the address is not known yet.
func (b *bench) probeLoop(ctx context.Context, rec *recorder, prefix string, url func() string) {
	for ctx.Err() == nil && !rec.has(prefix+".ok") {
		if u := url(); u != "" {
			b.probe(ctx, rec, prefix, u)
		}
		select {
		case <-ctx.Done():
		case <-time.After(b.c.interval):
		}
	}
}

func (b *bench) probe(ctx context.Context, rec *recorder, prefix, url string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	rec.mark(prefix + ".response")
	if resp.StatusCode == http.StatusOK && len(body) > 0 && (b.c.expect == "" || strings.Contains(string(body), b.c.expect)) {
		rec.mark(prefix + ".ok")
	}
}

// watchFrom lists to get a resource version and watches from it.
func watchFrom(ctx context.Context, list func(context.Context) (string, error), w func(context.Context, string) (watch.Interface, error), handle func(watch.Event)) error {
	rv, err := list(ctx)
	if err != nil {
		return err
	}
	wi, err := w(ctx, rv)
	if err != nil {
		return err
	}
	go func() {
		defer wi.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-wi.ResultChan():
				if !ok {
					return
				}
				handle(ev)
			}
		}
	}()
	return nil
}

func rvOf(l interface{ GetResourceVersion() string }, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return l.GetResourceVersion(), nil
}

func (b *bench) startWatches(ctx context.Context, rec *recorder, tg *target) error {
	ns, name, sbName := b.c.namespace, b.name(), b.sandboxName()
	byName := func(n string) metav1.ListOptions {
		return metav1.ListOptions{FieldSelector: "metadata.name=" + n}
	}
	withRV := func(o metav1.ListOptions, rv string) metav1.ListOptions {
		o.ResourceVersion = rv
		return o
	}
	conditions := func(prefix string, u *unstructured.Unstructured) {
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			rec.mark(fmt.Sprintf("%s.%v=%v", prefix, m["type"], m["status"]))
			rec.mark(fmt.Sprintf("%s.%v=%v/%v", prefix, m["type"], m["status"], m["reason"]))
			if prefix == "sandbox" && m["reason"] == "CheckedOut" {
				rec.mark(markCheckedOut)
			}
		}
	}

	spinApps := b.dc.Resource(spinAppGVR).Namespace(ns)
	sandboxes := b.dc.Resource(sandboxGVR).Namespace(ns)
	pods := b.kc.CoreV1().Pods(ns)
	services := b.kc.CoreV1().Services(ns)
	slices := b.kc.DiscoveryV1().EndpointSlices(ns)
	events := b.kc.EventsV1().Events(ns)
	sliceOpts := metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + name}
	podSeen := func(p *corev1.Pod) {
		tg.mu.Lock()
		tg.podName = p.Name
		if p.Status.PodIP != "" {
			tg.podIP = p.Status.PodIP
		}
		tg.mu.Unlock()
		rec.mark("pod.owned")
		for _, c := range p.Status.Conditions {
			if c.Status == corev1.ConditionTrue {
				rec.mark("pod." + string(c.Type))
			}
		}
	}

	return errors.Join(
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(spinApps.List(ctx, byName(name))) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				return spinApps.Watch(ctx, withRV(byName(name), rv))
			},
			func(ev watch.Event) {
				if u, ok := ev.Object.(*unstructured.Unstructured); ok && ev.Type != watch.Deleted {
					rec.mark("spinapp.added")
					conditions("spinapp", u)
				}
			}),
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(sandboxes.List(ctx, byName(sbName))) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				return sandboxes.Watch(ctx, withRV(byName(sbName), rv))
			},
			func(ev watch.Event) {
				u, ok := ev.Object.(*unstructured.Unstructured)
				if !ok || ev.Type == watch.Deleted {
					return
				}
				rec.mark(markSandboxAdded)
				tg.mu.Lock()
				var early *corev1.Pod
				if tg.sandboxUID == "" {
					tg.sandboxUID = string(u.GetUID())
					early = tg.pending[tg.sandboxUID]
					tg.pending = nil
				}
				tg.mu.Unlock()
				if early != nil {
					podSeen(early)
				}
				if p, _, _ := unstructured.NestedString(u.Object, "status", "phase"); p != "" {
					rec.mark("sandbox.phase=" + p)
				}
				conditions("sandbox", u)
			}),
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(pods.List(ctx, metav1.ListOptions{})) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				return pods.Watch(ctx, metav1.ListOptions{ResourceVersion: rv})
			},
			func(ev watch.Event) {
				p, ok := ev.Object.(*corev1.Pod)
				// Only the new sandbox's pod, by owner UID: a terminating
				// pod of an earlier sandbox with the same name must not count.
				if !ok || ev.Type == watch.Deleted || p.DeletionTimestamp != nil {
					return
				}
				owner := sandboxOwner(p)
				if owner == "" {
					return
				}
				tg.mu.Lock()
				uid := tg.sandboxUID
				if uid == "" {
					tg.pending[owner] = p
				}
				tg.mu.Unlock()
				if owner == uid {
					podSeen(p)
				}
			}),
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(services.List(ctx, byName(name))) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				return services.Watch(ctx, withRV(byName(name), rv))
			},
			func(ev watch.Event) {
				if ev.Type != watch.Deleted {
					rec.mark("service.added")
				}
			}),
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(slices.List(ctx, sliceOpts)) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				o := sliceOpts
				o.ResourceVersion = rv
				return slices.Watch(ctx, o)
			},
			func(ev watch.Event) {
				es, ok := ev.Object.(*discoveryv1.EndpointSlice)
				if !ok || ev.Type == watch.Deleted {
					return
				}
				pod, _ := tg.pod()
				for _, e := range es.Endpoints {
					if pod == "" || e.TargetRef == nil || e.TargetRef.Name != pod {
						continue
					}
					rec.mark("endpointslice.endpoint")
					if e.Conditions.Ready != nil && *e.Conditions.Ready {
						rec.mark(markEndpointsReady)
					}
				}
			}),
		watchFrom(ctx,
			func(ctx context.Context) (string, error) { return rvOf(events.List(ctx, metav1.ListOptions{})) },
			func(ctx context.Context, rv string) (watch.Interface, error) {
				return events.Watch(ctx, metav1.ListOptions{ResourceVersion: rv})
			},
			func(ev watch.Event) {
				e, ok := ev.Object.(*eventsv1.Event)
				if !ok || ev.Type != watch.Added || (e.Regarding.Name != name && e.Regarding.Name != sbName) {
					return
				}
				rec.mark("event." + e.Regarding.Kind + "." + e.Reason)
				if e.Regarding.Kind == "SwiftSandbox" && e.Reason == "CheckedOut" {
					rec.mark(markCheckedOut)
				}
			}),
	)
}

// sandboxOwner returns the UID of the SwiftSandbox that controls the pod.
func sandboxOwner(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Kind == "SwiftSandbox" && o.Controller != nil && *o.Controller {
			return string(o.UID)
		}
	}
	return ""
}

// cleanup deletes the benchmark SpinApp, if startupbench created it, and
// waits until the SpinApp, its sandboxes, their pods (including terminating
// ones) and its Service are gone. A Service still routes to a terminating
// pod when no ready one exists, which would make the next run's Service
// response come from the previous sandbox.
func (b *bench) cleanup(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	ns, name := b.c.namespace, b.name()
	spinApps := b.dc.Resource(spinAppGVR).Namespace(ns)
	cur, err := spinApps.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case cur.GetLabels()[createdByLabel] != createdByValue:
		return fmt.Errorf("SpinApp %s/%s exists and was not created by startupbench (no %s=%s label); choose another -name", ns, name, createdByLabel, createdByValue)
	default:
		// The UID precondition makes sure the object checked above is the
		// one deleted.
		fg, uid := metav1.DeletePropagationForeground, cur.GetUID()
		opts := metav1.DeleteOptions{PropagationPolicy: &fg, Preconditions: &metav1.Preconditions{UID: &uid}}
		if err := spinApps.Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for {
		left, err := b.leftovers(ctx)
		if err != nil {
			return err
		}
		if left == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to be deleted: %w", left, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// leftovers names one object from an earlier run that still exists, or "".
func (b *bench) leftovers(ctx context.Context) (string, error) {
	ns, name := b.c.namespace, b.name()
	if _, err := b.dc.Resource(spinAppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return "SpinApp " + name, err
	}
	if _, err := b.kc.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return "Service " + name, err
	}
	sbs, err := b.dc.Resource(sandboxGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, sb := range sbs.Items {
		if replicaOf(sb.GetName(), name) && ownerNamed(sb.GetOwnerReferences(), "SpinApp", name) {
			return "SwiftSandbox " + sb.GetName(), nil
		}
	}
	pods, err := b.kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		for _, o := range p.OwnerReferences {
			if o.Kind == "SwiftSandbox" && replicaOf(o.Name, name) {
				return "Pod " + p.Name, nil
			}
		}
	}
	return "", nil
}

// replicaOf reports whether sandbox is a replica name of the SpinApp,
// "<spinapp>-<ordinal>". Another SpinApp whose name starts with the same
// prefix ("<spinapp>-x") does not match.
func replicaOf(sandbox, spinApp string) bool {
	ord, ok := strings.CutPrefix(sandbox, spinApp+"-")
	if !ok || ord == "" {
		return false
	}
	for _, r := range ord {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func ownerNamed(refs []metav1.OwnerReference, kind, name string) bool {
	for _, o := range refs {
		if o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}

// waitWarmSlot waits until the pool has a warm slot whose pod started at
// least -min-slot-age ago.
func (b *bench) waitWarmSlot(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	sel := fmt.Sprintf("%s=%s,%s=warm", poolLabel, b.c.warmPool, slotStateLabel)
	for {
		pods, err := b.kc.CoreV1().Pods(b.c.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return err
		}
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil && p.Status.StartTime != nil && time.Since(p.Status.StartTime.Time) >= b.c.minSlotAge {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no warm slot in pool %s older than %s: %w", b.c.warmPool, b.c.minSlotAge, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

# Security review — 2026-09-19

First security pass over the wavekube operator (controller-runtime manager for
the GNodeB / RANPipeline / RANSecurityPolicy CRDs). Eight-vector enumeration with
exposure verdicts; fixed items cite the commit, deferred items say why.

## Summary

| # | Vector | Verdict | Action |
|---|--------|---------|--------|
| 1 | AuthN/AuthZ | **EXPOSED** (metrics) | **FIXED** `b823d17` — metrics bound to loopback |
| 2 | Injection | not exposed | none needed |
| 3 | Transport & secrets | not exposed | none needed (clean) |
| 4 | Input handling & DoS | not exposed | none needed |
| 5 | Supply chain | **EXPOSED** | **FIXED** `ecaddfc` — x/text→v0.39.0, go 1.22→1.26.1 |
| 6 | Data exposure | not exposed | none needed |
| 7 | Concurrency & state | not exposed | none needed |
| 8 | Infra & config | **EXPOSED** | **FIXED** `781a5f5` — RBAC least-privilege; workload hardening documented |

## 1 — AuthN/AuthZ — FIXED (`b823d17`)

The manager's **metrics endpoint bound `0.0.0.0:8080` as plaintext HTTP with no
authn/authz**, so any pod or host that could reach the pod IP scraped operator
metrics anonymously. **Fixed by binding it to `127.0.0.1`** (code default +
Helm `--metrics-bind-address=127.0.0.1:8080`, metrics containerPort unpublished),
which restricts it to the pod's own network namespace — remote anonymous access
closed with zero new dependencies and no functional loss, since nothing scrapes
it today (no Service/ServiceMonitor is wired).

**Deliberately chose loopback over the controller-runtime FilterProvider**
(`filters.WithAuthenticationAndAuthorization` + `SecureServing`): that path pulls
~15 transitive modules (`k8s.io/apiserver`, `component-base`, `cel-go`, otel,
grpc, apiserver-network-proxy) and adds `tokenreviews`/`subjectaccessreviews`
RBAC, all to authenticate scrapes that nothing performs yet — a supply-chain
surface increase for an unused capability, which cuts against the rest of this
pass. **Follow-up when Prometheus is wired:** front metrics with a kube-rbac-proxy
sidecar, or enable the FilterProvider + its RBAC then. Health/readiness on `:8081`
are anonymous by design (probe endpoints). No admission webhooks, no pprof. CRD
reconciliation is cluster-scoped, normal for an operator.

## 2 — Injection — NOT EXPOSED

No SQL/Mongo. No `os/exec`, no shell. Falco rule bodies are static string
literals; the only CR field reaching a created object's identity is `policy.Name`
in a ConfigMap name (k8s-validated) — no CR field is interpolated into rule
content, so no rule-injection vector (confirmed in prior work too).

## 3 — Transport & secrets — NOT EXPOSED (clean)

No controller reads, creates, or mounts `Secret` objects; the ClusterRole has no
`secrets` verb; no credential/token/key is logged. `RANSecurityPolicySpec` carries
only policy flags/paths. Caveat, not a leak: `EncryptFronthaul bool` is declared
but inert — no key material or key management exists behind it (documented so it
is not mistaken for a working control).

## 4 — Input handling & DoS — NOT EXPOSED

Inputs are Kubernetes CRs, validated/bounded by the apiserver and the CRD schema
before the controller sees them. No HTTP request body parsing. No unbounded
client-driven allocation path.

## 5 — Supply chain — FIXED (`ecaddfc`)

`golang.org/x/text` was **v0.14.0 — 25 minor versions behind**, below the v0.39.0
fix for **GO-2026-5970**; bumped to v0.39.0. The `go` directive was `1.22.0`
(four minor versions behind aerial's 1.26.1), which can silently miss toolchain-
level hardening — raised to `1.26.1`. Both verified: build + controller tests
green, x/text is indirect so no API surface change. **Still open (documented):**
the manager image is `:latest` (`values.yaml`) — floating, not digest-pinned;
recommend a pinned tag/digest. CR-supplied workload images (`gnb.Spec.Image`,
`pipeline.Spec.Image`) are used verbatim and the allowed-registry check is
**prefix-match, log-only and non-blocking** (`gnodeb_controller.go`) — a
disallowed-registry image is still deployed. Recommend making the registry check
blocking (fail the reconcile / set a NotReady condition) and requiring digests.

## 6 — Data exposure — NOT EXPOSED

No secrets handled or logged (see #3). Status/condition messages carry only
resource names and phases.

## 7 — Concurrency & state — NOT EXPOSED

Reconcilers are the standard controller-runtime single-worker-per-object model;
state lives in the apiserver and is mutated via `CreateOrUpdate`/status updates
with optimistic concurrency. Prior work already made terminal phases non-
requeuing (no hot loop) and the NetworkPolicy drift correction spec-based
(idempotent). No shared mutable in-process state.

## 8 — Infra & config — FIXED (`781a5f5`, partial)

**Fixed:** the operator ClusterRole carried unused grants — cluster-wide `pods`
full CRUD (never used; the PodTemplateSpecs live inside the Deployments/Jobs whose
own controllers create the pods), `events` create/patch (no EventRecorder), and
`delete` on services/configmaps (only ever Created/CreateOrUpdated). All removed;
least privilege verified via `helm template`.

**Still open (documented):**
- **Created GNodeB workload adds `NET_ADMIN` + sets `hostNetwork` from
  `GPUResources.EnableRDMA`** (the sample enables it), a real node-network
  escalation surface. `NET_ADMIN`/hostNetwork are plausibly required for RDMA
  fronthaul, so this is a functional trade-off to review with the RAN topology,
  not a safe blind removal. Neither created workload sets `seccompProfile:
  RuntimeDefault` — a safe hardening to add. The manager Deployment sets
  `runAsNonRoot`/`drop ALL`/`no-privilege-escalation` but lacks
  `readOnlyRootFilesystem` and `seccompProfile`.
- **Fronthaul NetworkPolicy has empty peers**: `desiredFronthaulPolicySpec` rules
  specify only `Ports` with no `From`/`To`, which in NetworkPolicy semantics means
  **all sources/destinations** on those ports (44000/UDP eCPRI, 9090/TCP metrics
  ingress; 53/UDP + 44000/UDP egress). The CR already declares `midhaulCIDR`/
  `backhaulCIDR` fields that are **currently unused** — the design-intended fix is
  to wire those CIDRs into ingress `ipBlock` peers (and restrict 9090 to the
  monitoring namespace). Not done blind this pass because over-restricting the
  fronthaul peer could break real eCPRI traffic from an external/hostNetwork O-RU;
  it needs validation against the actual fronthaul topology.

**Fronts fixed: 1 (metrics loopback `b823d17`), 5 (supply chain `ecaddfc`), 8
(RBAC least-privilege `781a5f5`). Deferred with reasons: the workload
securityContext / NetworkPolicy-peer items under 8 — each a functional trade-off
(RDMA capabilities, fronthaul reach) that must be validated against the RAN
topology rather than removed blind; and scrape-time metrics authz, which waits on
Prometheus actually being wired (see vector 1).**

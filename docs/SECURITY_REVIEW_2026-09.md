# Security review — 2026-09-19

First security pass over the wavekube operator (controller-runtime manager for
the GNodeB / RANPipeline / RANSecurityPolicy CRDs). Eight-vector enumeration with
exposure verdicts; fixed items cite the commit, deferred items say why.

## Summary

| # | Vector | Verdict | Action |
|---|--------|---------|--------|
| 1 | AuthN/AuthZ | partial (metrics) | documented (below) |
| 2 | Injection | not exposed | none needed |
| 3 | Transport & secrets | not exposed | none needed (clean) |
| 4 | Input handling & DoS | not exposed | none needed |
| 5 | Supply chain | **EXPOSED** | **FIXED** `ecaddfc` — x/text→v0.39.0, go 1.22→1.26.1 |
| 6 | Data exposure | not exposed | none needed |
| 7 | Concurrency & state | not exposed | none needed |
| 8 | Infra & config | **EXPOSED** | **FIXED** `781a5f5` — RBAC least-privilege; workload hardening documented |

## 1 — AuthN/AuthZ — partial (documented)

The manager's **metrics endpoint binds `:8080` as plaintext HTTP with no authn/
authz** — no `SecureServing`, no `FilterProvider`
(`filters.WithAuthenticationAndAuthorization`). Anyone able to reach the pod port
reads operator metrics anonymously. Health/readiness on `:8081` are anonymous by
design (probe endpoints). No admission webhooks, no pprof. Fix: enable the
controller-runtime metrics filter + serve over TLS, which also needs the
`authentication.k8s.io`/`authorization.k8s.io` (tokenreviews/subjectaccessreviews)
RBAC — a coupled change worth its own commit; recommended, not done this pass.
CRD reconciliation itself is not tenant-scoped (cluster-scoped operator), which
is normal for an operator.

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

**Front fixed here: 5 and 8. Deferred with reasons: 1 (metrics auth — coupled to
new RBAC, own commit), and the workload securityContext / NetworkPolicy-peer
items under 8 — each a functional trade-off (RDMA capabilities, fronthaul reach)
that must be validated against the RAN topology rather than removed blind.**

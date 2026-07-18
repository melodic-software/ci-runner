# CI runner alignment audit — report

Full-surface audit of the melodic-software self-hosted CI system (ci-runner controller, provisioning, ci-workflows selector, standards runner-policy, github-iac routing governance) against GitHub's official documentation, per the Brief in `PLAN.md`. Audit date: 2026-07-18.

## Headline

The fleet is strongly aligned with — and in several places stricter than — GitHub's current official guidance. The core architecture (ephemeral one-job JIT workers driven by the standalone `actions/scaleset` client outside Kubernetes) is a documented, supported path, not a divergence; ARC is GitHub's recommended solution *for Kubernetes*, which the fleet deliberately does not use. Both upstream pins are at latest (runner 2.335.1, scaleset v0.4.0), with image digests re-verified against the live registry. Live org security settings are all at GitHub's strictest values.

Of 12 divergence candidates detected during exploration: **1 withdrawn as a phantom** (D1), **6 confirmed** (recorded rationale survives first-principles re-derivation), and **5 need a decision**. The two IMPORTANT operational findings:

- **D11** — the 30-day rolling minimum-version rule converts the deliberate manual runner pin into a hard SLA, with a critical-security clause that bypasses the grace period entirely.
- **D12** — the queue monitor's own cron schedule is a silent single point of failure; its dominant failure mode (60-day public-repo auto-disable) defeats the control's purpose without emitting any error.

## Methodology and coverage

Three-stage pipeline: (1) a state inventory across the five governing repositories (ci-runner and its releases, provisioning, ci-workflows, standards runner-policy component, github-iac routing governance), sampled downstream consumer usage, live state on melo-desk-001 (controller, doctor output, Docker engine, image digests, versions), and GitHub API runner metadata for both hosts; (2) documentation-tier research across all six Brief tiers, every primary source fetched 2026-07-18, plus per-divergence verification agents re-deriving each candidate from first principles; (3) this report. melo-lap-001 was audited via repository state and GitHub API metadata only, per the Brief.

State-inventory baseline: a two-host Windows fleet; the Go controller (v0.1.18) uses GitHub App auth plus the official `actions/scaleset` client (v0.4.0) to run one-job JIT-registered ephemeral Linux workers as locked-down Docker containers under Docker Desktop/WSL2. Routing is a three-layer contract: the ci-workflows selector (three policies, fail-open hosted fallback), the standards runner-policy analyzer (managed component in five consumer repos), and github-iac Pulumi governance (two runner groups, org routing variables, `CI_RUNNER_POLICY=self-hosted-only` with an audited break-glass drift seam). Live state matched repo-declared state on every cross-checked value (release lock, worker digest, config, App identity).

**Erratum (D1 withdrawn).** The exploration stage initially flagged the selector's strict label allowlist as diverging from its README (one admitted label vs two documented routes). Verification showed the exploration read came from a feature branch, not `main`: on `main` and on both deployed selector pins, code, test, and README all agree on the two-label allowlist, and the fleet-label agreement test locks exactly those two labels. D1 is a phantom and is withdrawn; the lesson (verify branch context before trusting working-copy reads) was applied retroactively to the other repos, whose reports were corrected to `main` state.

**Verdict vocabulary** (from the Brief): **confirmed** (live rationale survives re-derivation), **unwarranted — fix**, or **needs-decision**. No finding was accepted merely because it is documented in-repo (reason-don't-recite); no candidate resolved to unwarranted-fix.

**Quote provenance.** Official-doc quotations in this report are close paraphrase from same-day fetches unless marked verbatim; anyone acting on exact wording should re-open the cited URL. The critical-security no-grace clause (D11) is deliberately paraphrased: two official sources state the same rule with different phrasing.

## Triage index

Ordered for the walkthrough: severity first, then decisions before confirmations.

| Priority | Finding | Verdict | Severity |
|---|---|---|---|
| 1 | [D11](#d11-runner-auto-update-disabled-with-manual-pin) runner minimum-version SLA + critical-CVE fast-path | confirmed posture; adopt-lean obligations | IMPORTANT |
| 2 | [D12](#d12-queue-monitor-schedule-guarantees) monitor scheduler SPOF / off-GitHub heartbeat | needs-decision | IMPORTANT |
| 3 | [D4](#d4-doctor-listener-acknowledgement-lag) doctor hard-fault vs expected-transient tension | needs-decision | MEDIUM |
| 4 | [D5](#d5-stale-mixed-selector-pins-in-consumers) selector-pin propagation posture | needs-decision | LOW |
| 5 | [D6](#d6-org-default-workflow-permissions-not-iac-pinned) workflow-permissions governance | needs-decision | LOW |
| 6 | [D10](#d10-shared-docker-engine-with-host-workloads) shared-engine residual naming | confirmed (defer) + one adopt item | SUGGESTION |
| 7 | [D2](#d2-review-tier-rollout-state) review-tier rollout | confirmed (complete) | — |
| 8 | [D3](#d3-observed-capacity-below-configured-maximum) memory-clamped capacity | confirmed (intentional) | — |
| 9 | [D7](#d7-fork-pr-approval-policy-not-iac-governed) fork-PR approval governance | confirmed (skip, trigger) | — |
| 10 | [D8](#d8-allowed-actions-unrestricted-org-wide) actions allowlist unused | confirmed (skip) | — |
| 11 | [D9](#d9-worker-passwordless-sudo) passwordless sudo residual | confirmed (accepted residual) | — |

## Divergence verdicts

### D2: Review-tier rollout state

**Verdict:** confirmed — the staged rollout is COMPLETE and live-verified, not mid-flight. **Severity:** none (residual cleanup only).

Exploration read a three-way state (IaC variable landed, provisioning routed claude-review, selector apparently not admitting the label). Re-derivation against `main`, the deployed selector pins, merged PR history, and live runs shows every gate met: the ci-workflows selector allowlist admits both labels (locked by test), the standards runner-policy accepts the review-tier variable as a governed selector input (merged via <https://github.com/melodic-software/standards/pull/155>), the org variable was provisioned by <https://github.com/melodic-software/github-iac/pull/141>, and provisioning wired claude-review through the governed selector in <https://github.com/melodic-software/provisioning/pull/146>. Live `CI_RUNNER_POLICY=self-hosted-only` plus consistently-succeeding selection jobs on 2026-07-18 runs prove the review label is admitted and routed end-to-end (under strict policy, the selection job can only succeed by returning an admitted label).

**Residual:** one stale code comment in github-iac (`OrgCiRouting.cs:79-82`) still describes the review variable as "inert until" conditions that are now met — a documentation-hygiene cleanup candidate in the owning repo.

### D3: Observed capacity below configured maximum

**Verdict:** confirmed — intentional and protocol-legal. **Severity:** none.

The documented scale-set target formula is `min(maxRunners, minRunners + TotalAssignedJobs)` (<https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets>; reference scaler in <https://github.com/actions/scaleset>). The controller instead advertises a memory-budget-clamped, burst-inventory-shaped capacity (observed 10 vs configured 12 under load) so the service never assigns jobs the single physical host cannot memory-fit. The listener protocol explicitly sanctions advertising a lower capacity mid-flight, so this is a legal, deliberate augmentation bound to the host's physical-memory model, not drift. It ties to the closed runner-performance work and needs no action.

### D4: Doctor listener acknowledgement lag

**Verdict:** needs-decision. **Severity:** MEDIUM.

The doctor's listener-health grace window is derived, not configured: request timeout + 2 × reconcile interval = 80 s, exactly reproducing the observed `grace=1m20s`. The acknowledgement timestamp pins while a capacity update stays unacknowledged, so exceeding grace means the GitHub listener has not acknowledged the currently-advertised capacity for a sustained, monotonically-growing period on a stable target — the observed 2 m 9 s lag was ~1.6× grace, not one in-flight poll. The scale-set protocol reference (<https://github.com/actions/scaleset>) never acknowledges capacity back to the service at all; the acknowledgement signal is a controller-derived convergence check for the multi-scale-set-per-host topology.

**The tension to resolve:** protocol-level analysis frames busy-fleet acknowledgement lag as expected-transient, but the doctor deliberately classifies it as a non-advisory hard fault that degrades the exit code — the only condition granted "expected operational state" status is a pending OS reboot. Both readings are internally consistent; the code does not answer which is right.

**Decision options (reserved for walkthrough):**

- (a) Reclassify the listener-acknowledgement check as advisory (WARN, never degrades exit) or widen the grace window — treats sustained lag under load as benign.
- (b) Keep the hard fault and treat sustained beyond-grace lag as a real defect signal worth investigating — treats the 2 m 9 s observation as actionable.

### D5: Stale mixed selector pins in consumers

**Verdict:** needs-decision. **Severity:** LOW (informational; nothing fails).

dotfiles and github-iac carry mixed, lagging selector pins (two SHAs within each repo). This is **not** a Dependabot defect: every consumer deliberately configures Dependabot to ignore the ci-workflows reusable-workflow ref, because an unreviewed bump could only fail the standards runner-policy lane — Dependabot would otherwise keep these refs current per <https://docs.github.com/en/code-security/dependabot/working-with-dependabot/keeping-your-actions-up-to-date-with-dependabot>. All pin bumps are manual, human-reviewed PRs; the within-repo non-uniformity is the signature of per-lane routing PRs that repinned some workflow files and not others. The lag is safe because the standards allowlist is additive — every lagging SHA is still approved, so nothing forces an update.

**The real finding:** no automation propagates a new selector SHA to consumer workflow pins when the allowlist advances.

**Decision options (reserved for walkthrough):**

- (a) Accept manual propagation lag as the intended posture (additive allowlist makes it safe).
- (b) Build a standards-driven repin mechanism that opens reviewed PRs across consumers when the approved-references list advances — also fixing within-repo non-uniformity.
- Enabling Dependabot for the selector ref is **not** a valid option: it fights the allowlist lockstep and would fail the runner-policy lane on any un-allowlisted SHA.

**Adjacent minor observation:** four consumers use a broad ignore pattern that also catches composite actions, contradicting their own "composites stay managed" comments — a separate one-line hygiene item. *(Erratum, post-walkthrough: execution-time history review showed the broad pattern is deliberate in all four — each repo widened it on purpose because ungoverned composite-action bumps drift adjacent pin-provenance comments (github-iac#89) — and only the comments were stale. The fix shipped as comment corrections, not pattern narrowing.)*

### D6: Org default workflow permissions not IaC-pinned

**Verdict:** needs-decision. **Severity:** LOW (governance drift-risk; not a security divergence).

The live org value is already GitHub's recommended strictest setting — default `GITHUB_TOKEN` permissions `read`, PR-approval by workflows disabled — matching the "good security practice" guidance at <https://docs.github.com/en/actions/reference/security/secure-use>. But the value is governed by nothing today: no Pulumi resource, no verifier check, no test assertion. A surgical single-purpose resource exists (`ActionsOrganizationWorkflowPermissions`, <https://www.pulumi.com/registry/packages/github/api-docs/actionsorganizationworkflowpermissions/>) that resets to the same secure defaults on destroy.

**Weighting caveat:** the github-iac pipeline runs `refresh:false` by design, so Pulumi pinning adds a declarative SSOT and reviewed change-control but **no live-drift detection** — an out-of-band UI change to a Pulumi-declared value stays invisible between refreshes. None of the org-level security values has a scheduled live-state drift detector today.

**Decision options (reserved for walkthrough; recommendation reserved):**

- (a) Pin via the surgical Pulumi resource — cleanest SSOT closure, low blast radius, matches the Pulumi-first posture.
- (b) Add one scheduled live governance-verify job covering this value, the fork settings, and the other verified org values at once — higher drift-assurance leverage than per-setting pinning.
- (c) Both — SSOT plus continuous assurance.

### D7: Fork-PR approval policy not IaC-governed

**Verdict:** confirmed — skip, with trigger. **Severity:** none.

Live org policy is already the strictest available (`all_external_contributors`), and no Terraform/Pulumi provider resource wraps the fork-PR contributor-approval API (provider gap; tracked publicly at <https://github.com/orgs/community/discussions/161687>), so IaC pinning is impossible today, not a design miss. The control is also largely moot for this fleet: it primarily guards public repos, while the fleet serves private repos only. **Trigger:** adopt when the provider ships the resource.

The load-bearing private-fork control, `members_can_fork_private_repositories=false` (live-verified), is expressible only via the monolithic `OrganizationSettings` resource (~30 unrelated fields; <https://www.pulumi.com/registry/packages/github/api-docs/organizationsettings/>). The existing fail-closed deploy-time verifier reads the live API value and blocks on drift — stronger at live-value assurance than refresh-off Pulumi. **Hold** the verify-don't-manage design; the monolithic resource is not worth one field.

### D8: Allowed actions unrestricted org-wide

**Verdict:** confirmed — skip. **Severity:** none.

Org policy is `allowed_actions: all` with `sha_pinning_required: true` (live-verified). The docs do not mandate allowlisting (<https://docs.github.com/en/actions/reference/security/secure-use>), and the two controls are orthogonal: SHA-pinning forces immutability of whatever runs; an allowlist gates which actions can be introduced at all. The actual introduction-gating substitute here is the standards runner-policy analyzer plus human PR review plus Dependabot structural-diff review — existing, reviewed control layers. A curated org allowlist would add constellation-wide maintenance burden for marginal assurance. Skip on that re-derived basis, not on incumbency.

### D9: Worker passwordless sudo

**Verdict:** confirmed — accepted residual. **Severity:** none.

GitHub's hardening documentation is silent on the runner OS user's sudo privileges; its least-privilege guidance targets workflow credentials (<https://docs.github.com/en/actions/reference/security/secure-use>). The worker runs non-root (uid 1001) with passwordless sudo — exact parity with the upstream `actions/runner` image (<https://github.com/actions/runner>) and GitHub-hosted runners. Inside an ephemeral, one-job, no-privileged/no-socket container the blast radius is a single throwaway job. Accepted residual; no doc divergence exists.

### D10: Shared Docker engine with host workloads

**Verdict:** confirmed — deferral sound; one cheap adopt item. **Severity:** SUGGESTION.

The premise that GitHub mandates a dedicated CI host was checked and **refuted**: neither the hardening page (<https://docs.github.com/en/actions/reference/security/secure-use>) nor the self-hosted runner reference (<https://docs.github.com/en/actions/reference/runners/self-hosted-runners>) contains any dedicated-machine rule — that framing is third-party. The official concern is minimizing sensitive information and network access reachable *from* the runner, and the measured surfaces are closed: dashboards publish loopback-only on a separate bridge network; workers hold no Docker socket (so cannot touch the shared image cache); digest pinning defeats cache masquerade; per-worker resource caps bound contention. The irreducible residual is a shared-kernel escape — the same residual any container-based runner carries, and exactly what the roadmap's threat-model-gated isolated-VM backend addresses.

**Adopt (cheap):** name "CI engine shared with non-CI host workloads" as an explicit accepted residual in the threat model, with a concrete revisit trigger — (i) a higher-value workload lands on the same engine, or (ii) concurrent cross-repo worker isolation becomes load-bearing.

**Deferred team-scale observation:** concurrent workers share the default bridge with inter-container communication enabled, so sibling workers can reach each other. Low severity today (single-tenant, first-party, equal-trust jobs); it is the finding that scales with multi-repo tenancy, and the isolated-VM backend also resolves it.

### D11: Runner auto-update disabled with manual pin

**Verdict:** confirmed posture; IMPORTANT operational obligation. **Severity:** IMPORTANT.

`DisableUpdate: true` plus a version- and digest-pinned runner image is GitHub's documented recommendation for container/ephemeral runners — rebuild on your own schedule. The pin (2.335.1) equals the latest upstream release, and the pinned digest was re-verified against the live registry. Two rules make this a hard SLA, per <https://docs.github.com/en/actions/reference/runners/self-hosted-runners> and <https://github.blog/changelog/2026-06-12-github-actions-minimum-version-enforcement-timeline-for-self-hosted-runners/>:

1. **30-day rolling rule (standing behavior, no platform qualifier):** with auto-update disabled, each new runner release — major, minor, or patch — must be installed within 30 days, or the service stops queuing jobs to the runner. The changelog dates brownouts only for GHEC (2026-09-25) and GHEC-DR (2026-07-31); no date is published for plain github.com Free/Team — but the undated part is the brownout schedule, not the rule, which applies to this Team org now.
2. **Critical-security clause (no grace):** when a critical security update is published, job queuing pauses immediately until the update is applied (paraphrase; both official sources confirm the substance with different wording).

Compliance today is incidental — the fleet happens to sit on latest. The moment a newer runner ships, a 30-day clock starts, and CI goes silent with no workflow-file error if the bump lands late. The registration floor (2.329.0) is cleared by six minors and never bites.

**Adopt-lean obligations (recommendations; walkthrough decides mechanism):**

- A monitored ≤30-day runner-release SLA: alert on each new `actions/runner` release and gate the release-lock freshness check against release age. Record explicitly that the existing 14-day drift-fail SLA beats the 30-day window with margin — currently that reconciliation is incidental, not recorded.
- An expedited critical-security rebuild fast-path in the release/drift runbook that bypasses the normal 24 h/7 d/14 d cadence (no qualifying CVE exists today — this is preparedness with a recorded trigger; the only advisory ever filed against the runner is a 2022 medium).

### D12: Queue monitor schedule guarantees

**Verdict:** needs-decision. **Severity:** IMPORTANT.

The monitor's self-documentation is accurate: GitHub documents no delivery guarantee for scheduled events — schedules can be delayed under load and queued invocations dropped (<https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows>). The design's core property holds: the monitor runs hosted, off the managed fleet, so fleet-down does not kill it.

**The residual:** the monitor's own schedule is an unmitigated, silent single point of failure. The dominant mode is the public-repo 60-day auto-disable rule — "in a public repository, scheduled workflows are automatically disabled when no repository activity has occurred in 60 days" (<https://docs.github.com/en/actions/how-tos/manage-workflow-runs/disable-and-enable-workflows>). The same public-repo choice that makes the control plane free subjects it to silent, persistent disablement, after which every fleet-down goes unalerted with no error emitted — defeating the control's purpose.

**Decision (reserved for walkthrough; recommendation reserved):** elevate the already-deferred off-GitHub dead-man's-switch/heartbeat — an external check the monitor pings on each successful run, alerting on *absence* of the ping, anchored off GitHub's scheduler. No GitHub-native mechanism provides this guarantee; the `workflow_job` webhook (<https://docs.github.com/en/webhooks/webhook-events-and-payloads>) is a separate, lower-priority latency lever (fires on `queued`, removing schedule-drop detection latency) but shares the GitHub dependency and does not close the both-dead gap.

## Aligned and closed findings

Verified aligned this audit — no action; recorded so the walkthrough need not revisit them.

- **Ephemeral one-job JIT model** — implements GitHub's explicit recommendation ("autoscaling with persistent self-hosted runners is not recommended"); the fresh container per job satisfies the JIT clean-environment caveat. <https://docs.github.com/en/actions/reference/runners/self-hosted-runners>
- **Scale-set client outside Kubernetes** — verbatim-verified: the client is "a standalone Go-based module … across VMs, containers, on-premise infrastructure, and cloud services"; ARC remains the recommended *Kubernetes* solution. The no-k8s non-goal is a documented supported path. <https://docs.github.com/en/actions/reference/runners/self-hosted-runners>, <https://github.com/actions/scaleset>
- **Scale-set protocol usage** — faithful and hardened: `TotalAssignedJobs` as sole scaling authority, capacity via the max-capacity header, acknowledge-before-acquire ordering preserved with crash-safe persistence added *before* the irreversible ack, JIT streamed over stdin (stricter than the reference's env-var path), explicit drain as the non-k8s analog of pod grace. <https://github.com/actions/scaleset>
- **No public scale-set REST** — the 404 on the org scale-set path is the documented state; the client is the only supported programmatic access outside Kubernetes. <https://docs.github.com/en/rest/actions/self-hosted-runners>
- **API version pin 2026-03-10** — live-confirmed the most recent supported version; none of its 26 breaking changes touch runner surfaces; pinning newest-explicit beats the older default. <https://docs.github.com/en/rest/about-the-rest-api/api-versions>
- **Session-token refresh intact** — the transport `RetryMax(0)` fail-fast does not suppress the library's 401 refresh-once path, which lives at the application layer; the option is redundant-defensive. Verified in the pinned module source. <https://github.com/actions/scaleset>
- **Worker teardown** — graceful retirement sends SIGTERM with an *unbounded* wait (no SIGKILL escalation at the Docker layer); SIGKILL exists only on the operator-invoked force-stop path, so the upstream signal-trap diagnostics flush is honored. Stronger than the finite grace the upstream contract assumes. <https://github.com/actions/runner>
- **Two-poll quiescence before deregistration** — internal-consistency verification, not an official-doc claim (no GitHub surface governs this controller-specific behavior): the controller README's documented fail-closed contract ("larger downscales and explicit zero-capacity modes retain the two-poll quiescence requirement before exact runner deregistration") was verified to match the reconciler code — two consecutive qualifying zero-capacity/zero-assignment polls gate retirement and deregistration, so assignment races fail closed.
- **Three-App least-privilege model** — host App holds exactly the one org permission scale-set admin and JIT require; routing-control holds exactly org Variables write; grants verified against live installation data. <https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api>, <https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/best-practices-for-creating-a-github-app>
- **Observer App four-scope grant is minimal** — an earlier trim-lean was *reversed* by verification against the authoritative fine-grained permission reference (parsed raw, summarizer-free): repo-scope runner listing requires Administration read; every scope has a code consumer or is GitHub's mandatory implicit metadata grant. No shipped doc carries a stale grant claim. <https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens>
- **Key management** — per-host unique App keys, DPAPI + BitLocker at rest, fingerprint algorithm matching GitHub's documented recipe exactly. <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps>
- **Rate limits** — installation and the separate runner-registration bucket fit the design with large headroom for a Team org; the transport parses Retry-After. <https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api>
- **Highest-severity hardening controls** — public-repo prohibition on runner groups (explicit `false` + selected visibility), read-only default token, strictest fork-PR approval, no-privileged/no-socket/no-mount isolation contract (code-enforced and live-confirmed), SHA-pinning required org-wide. <https://docs.github.com/en/actions/reference/security/secure-use>
- **Runner groups, labels, communication** — private-only selected-repo groups, single custom label (a valid documented pattern), outbound-443-only posture, service-mechanism latitude for the controller's scheduled-task launch, diagnostics extraction preserving the documented `_diag` model. <https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access>, <https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/configure-the-application>
- **Action-pin freshness dual coverage** — the repo's 12 SHA-pinned actions are covered by both a weekly grouped Dependabot github-actions ecosystem and the dependency-freshness check's version and tag-integrity loops. <https://docs.github.com/en/code-security/dependabot/working-with-dependabot/keeping-your-actions-up-to-date-with-dependabot>

## Unused features

Documented capabilities not in use, each with a disposition and basis. Dispositions marked *defer* carry a recorded trigger; adopt-leans are recommendations whose final call is reserved for the walkthrough.

| Feature | Disposition | Basis |
|---|---|---|
| Runner-group workflow restriction | **DEFER** | Weak defense-in-depth: the runner-policy analyzer already gates which workflows reach the fleet, and the feature is likely plan-gated (GHEC/GHES). Trigger: fleet opened to more repos, long-lived on-runner state, or selector-analyzer enforcement weakened. <https://github.blog/changelog/2022-03-21-github-actions-restrict-self-hosted-runner-groups-to-specific-workflows/> |
| Pre/post-job and container hooks | **SKIP** | Host-side config the container model doesn't expose; container hooks require Docker/k8s orchestration access that directly conflicts with the no-socket/no-privileged non-goals, and remain public preview. <https://docs.github.com/en/actions/reference/runners/self-hosted-runners> |
| Public per-runner registration/JIT/token REST | **SKIP** | Superseded by the scale-set client path; adopting would fork the fleet off its control plane. <https://docs.github.com/en/rest/actions/self-hosted-runners> |
| Runner-group provisioning via REST under github-iac | **ADOPT-lean (LOW)** | Group create/visibility/selected-repos is currently manual; the documented REST plus the existing App permission could bring it under IaC. Reserved for walkthrough. <https://docs.github.com/en/rest/actions/self-hosted-runner-groups> |
| workflow_job webhook | **SUGGESTION (latency only)** | Push-based queue signal; needs receiver infra; does not fix monitor liveness (see D12). Trigger: detection latency becomes load-bearing. <https://docs.github.com/en/webhooks/webhook-events-and-payloads> |
| Larger hosted runners | **SKIP** | Zero in use — the paid SKU is the cost surface the fleet exists to avoid. <https://docs.github.com/en/rest/actions/hosted-runners> |
| Worker network egress filtering | **DEFER** | Hosted runners ship a malicious-hosts blocklist; the worker bridge is unfiltered outbound. Genuine unused hardening, correctly gated behind the roadmap threat-model trigger. <https://docs.github.com/en/actions/reference/security/secure-use> |
| CodeQL workflow scanning | **SKIP** | zizmor + actionlint substitute (Actions-specialized SAST, arguably exceeding default-setup for workflow scanning). <https://docs.github.com/en/actions/reference/security/secure-use> |
| Runner exit code 7 as a health signal | **DEFER (LOW)** | The version-deprecated exit code could give the controller a distinct early warning that the pin aged past the minimum; cheap, complements D11. Trigger: first missed bump, or adopted alongside the D11 SLA. <https://github.com/actions/runner> |
| GHES multi-label scale sets | **N/A** | GHES-only capability. Trigger: any GHES move. <https://github.com/actions/scaleset> |
| OIDC cloud-credential exchange | **SKIP, defer** | Not applicable to runner registration; relevant only if a self-hosted workload starts deploying to a cloud provider — then adopt over long-lived secrets. <https://docs.github.com/en/actions/reference/security/secure-use> |
| External sign-only key vault | **SKIP, defer** | DPAPI + per-host unique keys + BitLocker is a coherent local-binding posture; a vault adds an external trust surface for marginal gain. Trigger: remote/HSM custody or centralized rotation need. <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps> |
| Per-token permissions down-scoping | **SKIP** | Moot: the separate single-purpose Apps achieve the same isolation at the installation layer. (The multi-App split itself is a reasoned first-principles position — GitHub documents least privilege but takes no stance on multi-App vs one-broad-App.) <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation> |
| Scale-set client post-v0.4.0 commits | **ADOPT on next tagged release** | Seven unreleased upstream commits, none protocol-semantic; carry the lock-contention, serialization, and credential-validation fixes when v0.5.0 tags; skip the mTLS-proxy change (no proxy in path). <https://github.com/actions/scaleset> |

## Limitations and observations

Carried forward honestly rather than laundered into findings:

- **Installation-token rate ceilings unmeasured (LOW).** The controller App's exact per-install limits were inferred from the documented rule plus Team-plan facts, not measured with an installation token; the headroom conclusion is unaffected at two-host volume. <https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api>
- **Out-of-scope observation:** intermittent claude-review *job* failures in dotfiles/github-iac runs are downstream review-execution issues on the review tier, not runner routing (selection jobs succeed; recent runs pass). Noted, not a routing defect.
- **Known not-covered remainders:** the exhaustive communication-domains table and IP-range endpoint (no egress allowlist is operated; trigger: adopting one); enterprise-level runner surfaces (Team org; trigger: GHEC move); which callers draw on the runner-registration rate bucket (informational); two secondary doc pages (workflow job-targeting syntax, runner removal) covered only indirectly.
- **melo-lap-001 live verification deferred** per the Brief; trigger: drift suspected from repo or API evidence.
- **Non-runner-surface hygiene items surfaced in passing:** the stale "inert until" comment (D2 residual) and the stale Dependabot ignore-pattern comments in four consumers (D5 note; the patterns themselves are deliberate — see the D5 erratum) belong to their owning repos at triage time.

## Walkthrough dispositions (2026-07-18)

One-by-one walkthrough completed over the triage index; verdicts above remain authoritative — this section records only the dispositions (fix now vs file issue vs accept) and owning repos.

| Finding | Disposition | Route |
|---|---|---|
| D11 | **Adopt, docs + runbook only.** The daily drift check already alerts within 24 h, hard-fails at ≥14 days from the first unadopted runner release (satisfying the 30-day rule with margin), and hard-fails on a critical/CVE-marked release at its next daily run — detection, not no-grace enforcement. The no-grace clause is met operationally by the expedited-rebuild fast-path added to the rolling-upgrade runbook, with GitHub's platform-side queuing pause as backstop. Record that reconciliation explicitly. No new automation. | ci-runner docs (this PR); provisioning runbook PR |
| D12 | **Adopt external dead-man's-switch heartbeat.** Monitor pings an off-GitHub check on each successful run; alert fires on absence. Covers the 60-day public-repo auto-disable, schedule drops, and the both-dead gap in one control. | Issue in the monitor's owning repo |
| D4 | **Widen the grace window; keep the hard fault.** Recalibrate the derived window so observed benign busy-fleet lag (~1.6× current grace) sits inside it; sustained beyond-grace lag stays a non-advisory defect signal. | ci-runner issue |
| D5 | **Accept manual propagation lag** (additive allowlist makes it safe). File a deferred repin-mechanism issue with triggers: allowlist pruning, consumer growth, or a first staleness-caused failure. The Dependabot ignore item reversed at execution time (see the D5 erratum): the broad patterns are deliberate; the stale comments were fixed instead. | standards issue; consumer comment-fix PRs |
| D6 | **Both, staged.** Pin now via the surgical `ActionsOrganizationWorkflowPermissions` resource (SSOT + change-control); separately file one scheduled live governance-verify job covering this value, the fork settings, and the other verified org values (drift assurance). | github-iac PR + github-iac issue |
| D10 | **Adopt the threat-model naming item.** "CI engine shared with non-CI host workloads" recorded as an explicit accepted residual with both revisit triggers, alongside the deferred inter-worker bridge-reachability observation. | ci-runner docs (this PR) |
| Runner-group IaC (unused features) | **Adopt via issue.** Bring group create/visibility/selected-repos under github-iac; scope the issue to also register group values in the D6 governance-verify job. | github-iac issue |
| D2, D3, D7, D8, D9 | **Confirmed as recorded** — no action beyond the already-queued D2 residual comment fix. | github-iac PR (D2 residual only) |
| Unused-features table | **Confirmed as recorded** — DEFER/SKIP entries stand with their triggers; scale-set client bump adopts on the next tagged release. | — |

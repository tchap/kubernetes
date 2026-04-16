# How to Write and Submit a Kubernetes Enhancement Proposal (KEP)

Sources:
- [KEP Process](https://github.com/kubernetes/enhancements/blob/master/keps/sig-architecture/0000-kep-process/README.md)
- [KEP Template](https://github.com/kubernetes/enhancements/blob/master/keps/NNNN-kep-template/)
- [Practical guide (K. Ashok)](https://medium.com/@kirtana.ashok/writing-kubernetes-enhancement-proposals-kep-12348e5c4cac)

---

## Before You Write Anything

### 1. Validate the idea informally

- Discuss the problem in the relevant SIG Slack channels (for this
  proposal: `#sig-node`, `#sig-network`, `#sig-apps`).
- Bring it up in SIG community sync meetings.
- File or reference a kubernetes/kubernetes issue describing the problem
  (issues #116965 and #124648 already exist for this case).
- Get a rough signal that the SIG agrees the work should happen.

### 2. Build a list of affected components

Map out every component that needs changes. For this proposal:
- kubelet (status manager)
- kube-controller-manager (endpointslice controller, endpoints controller)
- API types (no new types in Phase 1, but a new feature gate)

### 3. Identify collaborators and sponsors

- Find at least one SIG lead or approver willing to sponsor the KEP.
- Expect the process from KEP submission to GA to take 9-12 months
  (3 releases: Alpha -> Beta -> GA).
- Reviewers and approvers must be distinct from the KEP authors.

---

## Writing the KEP

### 4. Fork the enhancements repo

```
git clone https://github.com/kubernetes/enhancements.git
cd enhancements
```

### 5. Create the KEP directory

```
mkdir -p keps/sig-node/NNNN-disruption-target-signals-endpoint-terminating
```

The number (`NNNN`) is assigned after the initial PR is accepted. Use a
placeholder until then.

The owning SIG directory (`sig-node`) is whichever SIG is primarily
responsible. Since the kubelet change is the core of Phase 1, `sig-node`
is appropriate with `sig-network` and `sig-apps` as participating SIGs.

### 6. Create `kep.yaml`

This is the machine-readable metadata. Required fields:

```yaml
title: "DisruptionTarget Signals Endpoint Terminating"
kep-number: NNNN
authors:
  - "@your-github-handle"
owning-sig: sig-node
participating-sigs:
  - sig-network
  - sig-apps
status: provisional
creation-date: 2026-04-16
reviewers:
  - TBD
approvers:
  - TBD

stage: alpha
latest-milestone: "v1.36"
milestone:
  alpha: "v1.36"
  beta: "v1.37"
  stable: "v1.38"

feature-gates:
  - name: DisruptionTargetSignalsEndpointTerminating
    components:
      - kubelet
      - kube-controller-manager
disable-supported: true

metrics:
  - TBD
```

### 7. Create `README.md` from the template

Copy the template from `keps/NNNN-kep-template/README.md` and fill in
each section. The required sections are:

#### a. Summary
One paragraph. Should be usable as release notes.

#### b. Motivation
- The problem statement (traffic routed to dying pods during disruption).
- **Goals**: what this KEP achieves.
- **Non-Goals**: what it explicitly does not address (Phase 2 /
  PendingTermination is a non-goal for this KEP).

#### c. Proposal
- The concrete changes: kubelet sends DisruptionTarget early, endpoints
  controllers consume it.
- **User Stories**: "As a service owner, I want pods undergoing node
  shutdown to be removed from load balancer rotation before they stop."
- **Risks and Mitigations**: job controller impact, ecosystem consumers.

#### d. Design Details

- **Test Plan**: unit tests, integration tests, e2e tests. Must describe
  what exists and what will be added. Include links to test files.
- **Graduation Criteria**:
  - **Alpha**: feature gate off by default, basic unit + integration tests.
  - **Beta**: feature gate on by default, e2e tests pass, no negative
    feedback from Alpha users.
  - **GA**: feature gate locked on, confirmed working across major
    distributions, PRR approval.
- **Upgrade / Downgrade Strategy**: what happens when the gate is toggled.
  For this KEP: disabling the gate reverts to the current behavior
  (DisruptionTarget delayed). No persistent state changes.
- **Version Skew Strategy**: what if kubelet is newer than
  kube-controller-manager or vice versa? For this KEP: if the kubelet
  sends DisruptionTarget early but the controller doesn't consume it,
  there is no regression -- the condition is simply ignored. If the
  controller consumes it but the kubelet doesn't send it early, the
  controller sees no condition to act on -- also no regression.

#### e. Production Readiness Review (PRR) Questionnaire

This is a mandatory section with subsections. Answer each:

- **Feature Enablement and Rollback**: how to enable/disable (feature
  gate), what happens on rollback (reverts to current behavior).
- **Rollout, Upgrade, Rollback Planning**: can the feature be disabled
  without downtime? Yes -- toggling the gate requires a kubelet and
  controller-manager restart but does not affect running pods.
- **Monitoring Requirements**: what metrics or logs confirm the feature
  works? Existing `pod_status_sync_duration` metric tracks status update
  latency. Consider adding a metric for endpoints removed due to
  DisruptionTarget.
- **Dependencies**: none beyond existing APIs.
- **Scalability**: does this add API calls? One additional status PATCH
  per disrupted pod (sent earlier than before, not an extra call -- the
  call that was previously deferred is now sent sooner).
- **Troubleshooting**: how to debug if endpoints are not updating.

#### f. Alternatives
Reference the alternatives from the original proposal (delete pods,
override readiness, expand DisruptionTarget scope, do nothing).

#### g. Implementation History
Chronological log of major changes to the KEP.

---

## Submitting the KEP

### 8. Open the PR

```
git checkout -b kep-disruption-target-endpoints
git add keps/sig-node/NNNN-disruption-target-signals-endpoint-terminating/
git commit -m "KEP: DisruptionTarget signals endpoint terminating"
git push origin kep-disruption-target-endpoints
```

Open a PR against `kubernetes/enhancements` master branch. The PR should
contain only the KEP files -- no code changes.

Apply labels via comments on the PR:
```
/sig node
/sig network
/kind feature
```

### 9. Get provisional approval

- The owning SIG (sig-node) must accept the problem statement and agree
  the work should happen.
- Reviewers and approvers are assigned during triage.
- The KEP moves to `status: provisional`.

### 10. Iterate on design review

- Address reviewer feedback. Respond promptly -- delays compound near
  release deadlines.
- SIG leads from participating SIGs (sig-network) should review the
  endpointslice controller changes.
- The KEP is a living document; update it as the design evolves.

### 11. Get implementable approval

- Once approvers sign off, the KEP moves to `status: implementable`.
- This is the green light to write code.

### 12. Request PRR

- A member of the Production Readiness subproject (under sig-architecture)
  must approve the PRR questionnaire section.
- PRR is required before merging the implementation for each stage
  (Alpha, Beta, GA).
- PRR reviewers are listed in `keps/prod-readiness/` in the enhancements
  repo.

---

## Implementing the KEP

### 13. Write the code

- Implementation PRs go to `kubernetes/kubernetes` (not the enhancements
  repo).
- Reference the KEP number in PR descriptions.
- Feature-gate all new behavior.
- See `work/implementation-plan.md` for the detailed plan.

### 14. Track against the release milestone

- File an enhancement tracking issue if required by the release team.
- Code must be merged before the release code freeze date.
- Monitor the release schedule at
  `https://github.com/kubernetes/sig-release`.

### 15. Graduate through stages

Each stage requires:
- Updating `kep.yaml` (stage, milestone, status).
- Updating the KEP `README.md` with graduation criteria evidence.
- A new PRR approval for each stage.
- A PR to the enhancements repo documenting the promotion.

**Alpha -> Beta**: Gather feedback, confirm no regressions, ensure e2e
coverage. Change feature gate default to `true`.

**Beta -> GA**: Confirm stability over at least one release, lock the
feature gate to `true`, eventually remove the gate.

---

## Timeline Expectations

| Milestone | Typical Timeframe |
|-----------|-------------------|
| Idea discussion in SIG | 2-4 weeks |
| KEP PR opened | 1-2 weeks of writing |
| Provisional -> Implementable | 2-6 weeks of review |
| Alpha implementation merged | Within one release cycle |
| Beta promotion | Next release cycle |
| GA promotion | Release after Beta |
| Feature gate removal | 1-2 releases after GA |

Total: ~9-12 months from KEP to GA.

---

## Checklist

- [ ] Problem discussed in SIG meetings / Slack
- [ ] SIG sponsor identified
- [ ] `kep.yaml` created with all required fields
- [ ] `README.md` written from template with all sections
- [ ] PRR questionnaire answered
- [ ] PR opened against `kubernetes/enhancements`
- [ ] Labels applied (`/sig`, `/kind feature`)
- [ ] Provisional approval from owning SIG
- [ ] Design review from participating SIGs
- [ ] Implementable approval from approvers
- [ ] PRR approval
- [ ] Implementation PR(s) opened against `kubernetes/kubernetes`
- [ ] Code merged before code freeze
- [ ] KEP updated with implementation history

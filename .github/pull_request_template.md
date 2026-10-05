## What and why

## How it was tested

<!-- make verify, plus make verify-all, make kind-test or the KVM e2e test
where they apply. Say which tests ran; do not describe kind or envtest runs
as end-to-end tests. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`), see CONTRIBUTING.md
- [ ] New or changed SpinApp field handling is classified in
      `internal/compatibility` and `docs/compatibility.md`
- [ ] No secret value can reach an object, log, Event or status message
- [ ] Documentation and CHANGELOG.md are updated

# Pull Request Secret Review Policy

## Purpose

The workflow checks pull request changes for accidentally committed credentials
before the pull request is merged. It is a review aid, not a replacement for
revoking a credential that may already have been exposed.

## Detection rules

The deterministic scanner must inspect added lines in the pull request diff for:

- private key headers such as `-----BEGIN PRIVATE KEY-----`;
- provider and service token prefixes such as `ghp_`, `AKIA`, or `sk-live-`;
- assignments whose names contain `password`, `secret`, `token`, or `api_key`;
- connection strings containing an embedded password;
- Kubernetes `Secret` manifests with non-placeholder `data` or `stringData`;
- newly added `.env` files, except documented example files with placeholders.

Only added lines are findings. Removed lines and unchanged context are not
reported as newly introduced secrets.

## Verdict rules

- `block`: a probable real credential, private key, or non-placeholder
  Kubernetes Secret is present.
- `warn`: a suspicious value needs human review, but the value may be a test
  fixture, documentation example, or incomplete configuration.
- `pass`: no suspicious additions remain after deterministic scanning and
  contextual review.

The AI step may classify a scanner finding as a false positive only when the
surrounding context supports that conclusion. It must never include the full
suspected value in the report.

## Required report fields

Every report must contain:

1. pull request number and head commit SHA;
2. verdict and short rationale;
3. file and line for every finding;
4. detector that produced the finding;
5. masked value or a safe fingerprint, never the original value;
6. remediation advice;
7. whether the finding was classified as `block`, `warn`, or `false_positive`.

## Remediation

For a probable real credential:

1. block or do not merge the pull request;
2. revoke or rotate the credential;
3. remove it from the source and repository history when necessary;
4. replace it with a secret-manager or runtime-injected reference.

For a test or documentation example, replace realistic-looking values with
obvious placeholders such as `REPLACE_ME` or `sk-demo-not-real`.

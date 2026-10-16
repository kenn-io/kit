# Live object storage tests

Kit's ordinary PR checks use loopback fixtures and a disposable RustFS server.
They do not establish compatibility with real AWS S3 or Google Cloud Storage.
The CI workflow also has an opt-in GCS contract job for a maintainer-selected
branch. It runs the existing `TestGCSLiveContract` against Google's XML API.

The contract covers empty and small objects, multi-chunk resumable uploads,
checksums, identical retries, conflicting contents, revision-guarded reads and
deletes, paginated listing with escaped names, and a lost upload response.
It checks storage behavior, not application query performance.

## One-time GCS setup

Use a billing-enabled test project and a bucket containing only disposable test
data. The runner needs no Google project administration credentials. A maintainer
creates the resources once using `gcloud`; CI receives only the test account's
HMAC key. These [HMAC credentials](https://docs.cloud.google.com/storage/docs/authentication/hmackeys)
are separate from OAuth tokens and service-account JSON keys.

1. Create a dedicated service account with no project-wide roles.
2. Create a regional Standard storage bucket with uniform bucket-level access
   and public access prevention. Disable soft delete and leave object versioning
   and retention policies off for this disposable bucket.
3. Grant the service account `roles/storage.objectAdmin` on that bucket only.
4. Configure a lifecycle rule to delete objects under `ci/` after one day.
   Successful tests remove their verified objects. Failed or cancelled uploads
   can leave objects behind, so lifecycle deletion is the fallback. It is
   asynchronous and does not promise deletion at exactly 24 hours.
5. Create an HMAC key for the service account. Google returns its secret only
   when creating the key; send it directly to the GitHub environment secret
   below. Do not put it in a workflow file, issue, or command transcript.

For example, after setting `KIT_TEST_PROJECT`, `KIT_TEST_BUCKET` (a new, globally
unique name), and `KIT_TEST_REGION` in your shell:

```bash
gcloud iam service-accounts create kit-gcs-conformance --project="$KIT_TEST_PROJECT"
KIT_TEST_ACCOUNT="kit-gcs-conformance@${KIT_TEST_PROJECT}.iam.gserviceaccount.com"
gcloud storage buckets create "gs://$KIT_TEST_BUCKET" \
  --project="$KIT_TEST_PROJECT" --location="$KIT_TEST_REGION" \
  --default-storage-class=STANDARD --uniform-bucket-level-access \
  --public-access-prevention --soft-delete-duration=0
gcloud storage buckets add-iam-policy-binding "gs://$KIT_TEST_BUCKET" \
  --project="$KIT_TEST_PROJECT" --member="serviceAccount:$KIT_TEST_ACCOUNT" \
  --role=roles/storage.objectAdmin
```

Apply this JSON through `gcloud storage buckets update` with `--lifecycle-file`
and the explicit bucket and project:

```json
{"rule":[{"action":{"type":"Delete"},"condition":{"age":1,"matchesPrefix":["ci/"]}}]}
```

The setup operator needs permissions to create the service account, bucket,
bucket IAM binding, and HMAC key. HMAC administration requires
`storage.hmacKeys.create` (for example through `roles/storage.hmacKeyAdmin`);
the CI service account does not need that role. See Google's
[HMAC administration guide](https://docs.cloud.google.com/storage/docs/authentication/managing-hmackeys)
and [lifecycle guide](https://docs.cloud.google.com/storage/docs/lifecycle).

## GitHub environment

Create the `objectstore-gcs` environment under the repository's Settings →
Environments. Require a maintainer's approval and allow only `main` and the
specific branch being validated. Review that branch's code before approving
access to credentials. Remove temporary branch allowances after merge.
GitHub documents [environment approvals and branch restrictions](https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments).

Configure these environment values:

| Kind | Name | Value |
| --- | --- | --- |
| Variable | `KIT_GCS_TEST_BUCKET` | Dedicated test bucket name, without `gs://` |
| Secret | `KIT_GCS_TEST_KEY_ID` | Service account HMAC access ID |
| Secret | `KIT_GCS_TEST_SECRET` | Service account HMAC secret |

Use the environment UI or `gh variable set` / `gh secret set` with
`--repo kenn-io/kit --env objectstore-gcs`. `gh secret set` accepts the value
on stdin, so the secret need not appear in shell history. Allow up to a minute
for a new HMAC key to become usable before the first run.

## Run and interpret

Dispatch the existing CI workflow on the branch to validate:

```bash
gh workflow run ci.yml --repo kenn-io/kit --ref BRANCH -f gcs_live=true
```

Approve the environment job after checking the run's commit. The **Live GCS
contract** job uses `ci/<run ID>/<attempt>/<random nonce>` within the test bucket.
Missing configuration fails the enabled test. The test has a ten-minute limit;
the job has a fifteen-minute limit. Regular PRs and default manual runs skip this
job. Ordinary CI checks still run on the selected branch.

Keep the live job manual while establishing the first successful provider run.
Record its run URL and commit with the review evidence. A skipped job is not
provider validation. Once established, scheduled runs on `main` can detect
provider changes; do not make an unconfigured cloud job a required PR check.

Rotate the HMAC key by creating a replacement, updating the environment secrets,
and validating a run before deactivating and deleting the old key. When retiring
the job, deactivate and delete its HMAC keys, remove the environment credentials,
and delete the dedicated test bucket and service account.

Real AWS S3 validation is a separate follow-up requiring an AWS account and an
explicit live fixture. The current S3 fixture intentionally accepts loopback
endpoints only; a passing RustFS job does not count as AWS validation.

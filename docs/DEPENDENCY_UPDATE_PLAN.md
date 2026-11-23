# Automated dependency updates and image publishing

Renovate checks for new dependency versions once a day. Each update PR is tested against the current head of `main` and merges automatically when its required checks pass. Several PRs can merge on the same day. The `latest` tag is updated after the tested image has been published.

## Dependencies to track

- Track Go, the Go build image, and both direct and indirect Go modules. Test minor and patch updates to the AWS SDK for Go and Smithy together.
- Update `mongodump` and `mongorestore` together as part of the same MongoDB Database Tools distribution. Update the bundled version in the Dockerfile, installer, and README in the same PR.
- Track the Ubuntu base image, GitHub Actions, and the MongoDB, BusyBox, and Silo images used by Compose and the integration tests. Keep the MongoDB version in Compose and CI in sync with the default in the Go tests. Update the Silo tag and its digest together.

## Auto-merging multiple PRs

1. Check for new versions once a day. Require unit tests and static checks for every PR. On both amd64 and arm64, build the image and run the backup and restore integration tests.
2. Enable GitHub auto-merge. Configure branch protection to require each PR to be up to date with `main` and to pass its required checks before merging.
3. When a PR merges, the push to `main` triggers the `refresh` job. Using a token from the same bot as the daily run, the job updates the remaining open Renovate PR branches in the repository and triggers CI on their new commits. This lets those PRs proceed before the next daily check or the preceding PR's image publication.
4. Leave any PR with failing tests or an unresolved merge conflict open. Other PRs can merge as soon as their own required checks pass.

For a Database Tools update, create a backup with the previous version, restore it with the new version, and verify that the restored data matches the source. [MongoDB restore compatibility](https://www.mongodb.com/docs/database-tools/mongorestore/mongorestore-behavior-access-usage/)

## Publishing after merge and updating `latest`

1. For each commit that lands on `main`, build the amd64 and arm64 images and run the integration tests against each image.
2. Once both architectures pass, assemble the tested images into a multi-architecture image. Acquire the lock for the commit SHA and check whether its tag already exists. On the first successful publication, tag the tested digest as `sha-<commit>`. Reuse that digest for reruns and release tags.
3. Use a dedicated job to update `latest`. Use the same concurrency group for automatic runs, reruns, and manual runs, and keep pending jobs queued. After acquiring the lock, read the current head SHA of `main` from GitHub.
4. If the head SHA matches the commit being published, point `latest` at the digest published under the SHA tag. Hold the lock from the head check through the tag update. If the head has moved, leave the SHA tag in place and exit. If a newer commit fails testing or its SHA tag fails to publish, `latest` continues to point to the last successfully promoted image.

For example, suppose A merges before B, but B finishes building first and is promoted to `latest`. When A finishes, its head check sees B and exits. Rerunning A gives the same result. [Publishing workflow](../.github/workflows/verify.yml) · [GitHub Actions concurrency and queuing](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency)

## Rollout and acceptance criteria

1. Fix connectivity to the test MongoDB instance on the current `main` branch. Verify that backup and restore pass on both amd64 and arm64. [CI run that failed on both architectures during the investigation](https://github.com/ajctrl/mongodb-backup-s3/actions/runs/40920548360)
2. Implement the previous-version restore test and publish the tested image digest. Verify that rebuilding the same SHA preserves its published digest and that `latest` still points to the newer image when an older job finishes later or is rerun.
3. Verify that Renovate detects the dependencies listed above, then enable required checks and auto-merge. With two update PRs open, merge the first and verify that the second is brought up to date and retested the same day.

## Switching workflows on GitHub

1. On the PR that introduces `verify.yml`, confirm that `unit`, `Integration (amd64)`, `Integration (arm64)`, and `publication-test` pass. The publication test uses a disposable local registry.
2. Enable `Allow auto-merge` on GitHub. In the branch protection settings for `main`, select those four GitHub Actions checks as required and require branches to be up to date before merging. Apply the same protection rules to the bot, with no bypass permission.
3. Include `mongodb-backup-s3` in the repositories covered by the `ajctrl-sso-gateway-renovate` GitHub App installation and verify that the app has the permissions Renovate needs. Store the app's client ID in the Actions variable `RENOVATE_APP_CLIENT_ID` and its private key in the Actions secret `RENOVATE_APP_PRIVATE_KEY`. The daily update job and the PR refresh job each request an installation token scoped to this repository at runtime. [Renovate GitHub App permissions](https://docs.renovatebot.com/modules/platform/github/#running-as-a-github-app)
4. Immediately before switching, check the publishing runs for the old `test.yml` workflow. Wait for active runs to finish. If any runs are still eligible for a rerun, disable the old workflow before introducing the new one. The workflows have different filenames, so the old one can be disabled independently.
5. Renovate can be run manually to verify the initial setup. The regular check runs daily at 03:17 UTC. Publish to Docker Hub only when both existing Docker Hub secrets are configured.

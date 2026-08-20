# Sub2API repository workflow

## Repository topology

- `origin` is the project fork at `https://github.com/Synflux-AI/sub2api.git`.
- `upstream` is the source repository at `https://github.com/Wei-Shaw/sub2api.git`.
- `upstream` is read-only for this project. Never push branches, tags, or commits to it.
- `main` mirrors `upstream/main`. Do not develop, release, or deploy directly from `main`.
- `release` is this project's integration and release branch. Import upstream changes into it one way through local `main`.
- Do not create or use a local `dev` branch unless explicitly requested.

## Synchronizing upstream

- Before syncing, inspect `git status` and preserve unrelated local changes.
- Fetch from `upstream`, fast-forward local `main` to `upstream/main`, then merge local `main` into `release`.
- If either branch has diverged unexpectedly, stop and resolve the branch history deliberately; do not reset or discard commits.
- Push only to `origin` when the project workflow requires updating the project fork. Never push back to `upstream`.

## Development workflow

- Formal feature, bug, configuration, migration, deployment, and product-behavior changes use an existing Issue when one exists.
- Read-only investigation, review, diagnosis, and purely mechanical changes may skip Issue and branch ceremony.
- Start implementation from an up-to-date `release` in a working branch. Never edit or commit directly on `main` or `release`.
- Use `<type>/<short-description>` or `<type>/<issue-number>-<short-description>` with one of `feat`, `fix`, `docs`, `refactor`, `test`, `ci`, or `chore`. Use lowercase ASCII kebab-case and no username/tool prefix.
- PRs for project changes target the project's `release`; do not open reverse-contribution PRs to `Wei-Shaw/sub2api`.
- After merge, delete the remote branch and remove its local worktree after confirming it is clean.

## Documentation and design

- `AGENTS.md` is the repository workflow source of truth. `CLAUDE.md` is a symlink to this file.
- `README.md` documents user-facing setup and usage. `DEV_GUIDE.md` documents environment setup and troubleshooting.
- Do not create new `openspec/changes` artifacts. Existing files there are historical design records; use ordinary Markdown in `docs/` or the Issue/PR for new work.
- Update API, migration, deployment, or behavior documentation in the same change when it becomes stale.

## Validation

- Backend uses Go modules; frontend uses pnpm. Do not add npm, yarn, or Bun lockfiles or change the package manager without an explicit migration request.
- Format changed Go files with `gofmt`.
- Before a backend PR, run `cd backend && make test-unit`, `make test-integration`, `golangci-lint run ./...`, and `govulncheck ./...`.
- For frontend changes, run `cd frontend && pnpm install --frozen-lockfile`, the relevant lint/typecheck/tests, and `pnpm run build` when the bundle or embedded assets are affected.
- After Ent schema changes, run `go generate ./ent` and commit the generated files. Migration changes require a disposable-database smoke test.
- Deployment changes require the relevant Compose or shell validation and a focused local smoke test. GitHub Actions is authoritative for CI gates; Jenkins is not a second CI system.

## CI, release, and deployment

- GitHub Actions owns pull-request CI, security scanning, and production image publication. Do not duplicate those jobs in Jenkins.
- Production releases originate only from a protected `vX.Y.Z` tag whose commit belongs to `origin/release` and has passed the required GitHub Actions checks.
- Publish only the Linux `amd64` image to `ghcr.io/synflux-ai/sub2api`, using the immutable version tag. Do not publish binary archives, multi-architecture images, Docker Hub images, or a second copy of the image unless explicitly requested.
- Release automation must not commit generated version changes to `main`; `main` remains an exact mirror of `Wei-Shaw/sub2api`.
- Backend production deployment is a separate, manually triggered Jenkins job. It pulls an already-published immutable GHCR version or digest, updates the Docker Swarm service, and verifies rollout health.
- Jenkins must not rebuild or republish the backend image, rerun CI, deploy from source, or deploy automatically on branch pushes or tags. Do not deploy `latest` to production.

## Security

- Never commit or expose secrets in source, responses, logs, fixtures, environment files, or audit metadata. Sanitize sensitive upstream payloads before logging.
- Changes to client wire behavior, streaming, provider compatibility, authorization, data access, migrations, or release/deployment require focused regression coverage and review.
- Do not manually publish production artifacts or bypass the Jenkins deployment gate.

## Git conventions

- Commit titles and bodies use Chinese. Technical identifiers may remain in English.
- Do not commit generated frontend output, local secrets, test databases, build caches, or local deployment overrides.

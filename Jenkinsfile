// =============================================================================
// Sub2API CI/CD — Jenkins Declarative Pipeline
// -----------------------------------------------------------------------------
// Migrated from GitHub Actions (backend-ci / security-scan / release).
//
// Triggers (configured on the job): GitHub push webhook on
//   - branch `release`      -> run CI gate only
//   - tag    `v*`           -> run CI gate, then build & push image to GHCR
//
// The host has no Go/pnpm toolchain, so every test/lint/scan stage runs inside
// an ephemeral Docker container (workspace bind-mounted, caches in named
// volumes). Only the image build talks to the host Docker daemon directly.
//
// CD is push-only: build the image and push to GHCR. No deployment.
// =============================================================================

pipeline {
  agent any

  options {
    disableConcurrentBuilds()
    buildDiscarder(logRotator(numToKeepStr: '30', artifactNumToKeepStr: '10'))
    timestamps()
    timeout(time: 60, unit: 'MINUTES')
  }

  environment {
    GHCR_IMAGE    = 'ghcr.io/synflux-ai/sub2api'
    GO_IMAGE      = 'golang:1.26.4-alpine'
    GO_FULL_IMAGE = 'golang:1.26.4'
    NODE_IMAGE    = 'node:20-alpine'
    LINT_IMAGE    = 'golangci/golangci-lint:v2.9-alpine'
    PY_IMAGE      = 'python:3-slim'
    GOPROXY       = 'https://goproxy.cn,direct'
    GOSUMDB       = 'sum.golang.google.cn'
    // Frontend "critical" vitest suite — kept in sync with root Makefile.
    FE_CRITICAL   = 'src/views/auth/__tests__/LinuxDoCallbackView.spec.ts src/views/auth/__tests__/WechatCallbackView.spec.ts src/views/user/__tests__/PaymentView.spec.ts src/views/user/__tests__/PaymentResultView.spec.ts src/components/user/profile/__tests__/ProfileInfoCard.spec.ts src/views/admin/__tests__/SettingsView.spec.ts'
  }

  stages {
    // -----------------------------------------------------------------------
    // Detect whether this checkout sits on a release tag (v*). Reliable in a
    // plain (non-multibranch) pipeline where buildingTag()/TAG_NAME are unset.
    // -----------------------------------------------------------------------
    stage('Detect ref') {
      steps {
        script {
          sh 'git fetch --tags --quiet || true'
          env.RELEASE_TAG = sh(
            returnStdout: true,
            script: "git tag --points-at HEAD | grep -E '^v[0-9]' | head -n1 || true"
          ).trim()
          if (env.RELEASE_TAG) {
            echo "Building release tag ${env.RELEASE_TAG} -> image will be built & pushed"
          } else {
            echo "No release tag at HEAD -> CI gate only, no image push"
          }
        }
      }
    }

    // -----------------------------------------------------------------------
    // CI gate — unit tests + lint + security scan. All in containers, parallel.
    // -----------------------------------------------------------------------
    stage('CI') {
      parallel {
        stage('Backend unit tests') {
          steps {
            sh '''
              docker run --rm \
                -v "$WORKSPACE":/w -w /w/backend \
                -v jenkins-sub2api-gomod:/go/pkg/mod \
                -v jenkins-sub2api-gocache:/root/.cache/go-build \
                -e GOPROXY -e GOSUMDB \
                "$GO_IMAGE" sh -c 'apk add --no-cache make >/dev/null && make test-unit'
            '''
          }
        }

        stage('golangci-lint') {
          steps {
            sh '''
              docker run --rm \
                -v "$WORKSPACE":/w -w /w/backend \
                -v jenkins-sub2api-gomod:/go/pkg/mod \
                -v jenkins-sub2api-gocache:/root/.cache/go-build \
                -v jenkins-sub2api-golangci:/root/.cache/golangci-lint \
                -e GOPROXY -e GOSUMDB \
                "$LINT_IMAGE" golangci-lint run --timeout=30m
            '''
          }
        }

        stage('govulncheck') {
          steps {
            sh '''
              docker run --rm \
                -v "$WORKSPACE":/w -w /w/backend \
                -v jenkins-sub2api-gomod:/go/pkg/mod \
                -v jenkins-sub2api-gocache:/root/.cache/go-build \
                -v jenkins-sub2api-gobin:/go/bin \
                -e GOPROXY -e GOSUMDB \
                "$GO_FULL_IMAGE" sh -c 'go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./...'
            '''
          }
        }

        stage('Frontend unit + lint') {
          steps {
            sh '''
              docker run --rm \
                -v "$WORKSPACE":/w -w /w/frontend \
                -v jenkins-sub2api-pnpm:/pnpm-store \
                "$NODE_IMAGE" sh -c '
                  corepack enable &&
                  corepack prepare pnpm@9 --activate &&
                  pnpm config set store-dir /pnpm-store &&
                  pnpm install --frozen-lockfile &&
                  pnpm run lint:check &&
                  pnpm run typecheck &&
                  pnpm exec vitest run '"$FE_CRITICAL"'
                '
            '''
          }
        }

        stage('Frontend audit') {
          steps {
            sh '''
              docker run --rm \
                -v "$WORKSPACE":/w -w /w/frontend \
                -v jenkins-sub2api-pnpm:/pnpm-store \
                "$NODE_IMAGE" sh -c '
                  corepack enable &&
                  corepack prepare pnpm@9 --activate &&
                  pnpm config set store-dir /pnpm-store &&
                  pnpm install --frozen-lockfile &&
                  pnpm audit --prod --audit-level=high --json > audit.json || true
                '
              docker run --rm \
                -v "$WORKSPACE":/w -w /w \
                "$PY_IMAGE" python tools/check_pnpm_audit_exceptions.py \
                  --audit frontend/audit.json \
                  --exceptions .github/audit-exceptions.yml
            '''
          }
        }
      }
    }

    // -----------------------------------------------------------------------
    // Build & push image — only when HEAD is a release tag (v*).
    // Uses the self-contained root Dockerfile (amd64 only). Push to GHCR.
    // -----------------------------------------------------------------------
    stage('Build & Push image') {
      when { expression { return env.RELEASE_TAG?.trim() } }
      steps {
        script {
          def version = env.RELEASE_TAG.replaceFirst(/^v/, '')
          def commit  = sh(returnStdout: true, script: 'git rev-parse --short HEAD').trim()
          def date    = sh(returnStdout: true, script: 'date -u +%Y-%m-%dT%H:%M:%SZ').trim()
          withCredentials([usernamePassword(
              credentialsId: 'ghcr-registry',
              usernameVariable: 'REG_USER',
              passwordVariable: 'REG_PASS')]) {
            sh """
              echo "\$REG_PASS" | docker login ghcr.io -u "\$REG_USER" --password-stdin
              docker build \
                --build-arg VERSION=${version} \
                --build-arg COMMIT=${commit} \
                --build-arg DATE=${date} \
                -t ${GHCR_IMAGE}:${version} \
                -t ${GHCR_IMAGE}:latest \
                -f Dockerfile .
              docker push ${GHCR_IMAGE}:${version}
              docker push ${GHCR_IMAGE}:latest
              docker logout ghcr.io || true
            """
          }
          echo "Pushed ${GHCR_IMAGE}:${version} and :latest"
        }
      }
    }
  }

  // -------------------------------------------------------------------------
  // Feishu notification (success/failure). Webhook stored as a Jenkins secret.
  // -------------------------------------------------------------------------
  post {
    success { script { notifyFeishu('success') } }
    failure { script { notifyFeishu('failure') } }
  }
}

def notifyFeishu(String result) {
  def ref = env.RELEASE_TAG?.trim() ? "tag ${env.RELEASE_TAG}" : (env.GIT_BRANCH ?: 'release')
  def pushed = env.RELEASE_TAG?.trim() ? "\\n镜像: ${env.GHCR_IMAGE}:${env.RELEASE_TAG.replaceFirst(/^v/, '')}" : ''
  def emoji = result == 'success' ? '✅' : '❌'
  def title = "${emoji} Sub2API CI/CD ${result == 'success' ? '成功' : '失败'}"
  def text  = "${title}\\n分支/引用: ${ref}\\n构建: #${env.BUILD_NUMBER}${pushed}\\n${env.BUILD_URL}"
  try {
    withCredentials([string(credentialsId: 'sub2api-feishu-webhook', variable: 'FEISHU_URL')]) {
      sh """
        curl -s -X POST "\$FEISHU_URL" \
          -H 'Content-Type: application/json' \
          -d '{"msg_type":"text","content":{"text":"${text}"}}' || true
      """
    }
  } catch (ignored) {
    echo 'Feishu webhook credential not configured; skipping notification.'
  }
}

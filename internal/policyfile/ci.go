package policyfile

import (
	"fmt"
	"strings"

	"github.com/patrikmichi/zuano/internal/brand"
)

const githubWorkflow = `# Plans a policy change on every pull request and applies it when it is merged.
# The job signs in with GitHub's OIDC token: no secret is stored. Set the
# repository variable {{ENV}}_URL to the gateway's address.
name: Policy
on:
  pull_request:
    paths: ["{{DIR}}/**"]
  push:
    branches: [main]
    paths: ["{{DIR}}/**"]
permissions:
  contents: read
  id-token: write
  pull-requests: write
jobs:
  policy:
    runs-on: ubuntu-latest
    env:
      {{ENV}}_URL: ${{ vars.{{ENV}}_URL }}
      GH_TOKEN: ${{ github.token }}
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - run: go install github.com/patrikmichi/{{CMD}}/cmd/{{CMD}}@latest
      - name: Plan
        if: github.event_name == 'pull_request'
        run: |
          {{CMD}} policy plan --dir {{DIR}} --format markdown --out plan.md
          cat plan.md >> "$GITHUB_STEP_SUMMARY"
          gh pr comment ${{ github.event.pull_request.number }} --edit-last --create-if-none --body-file plan.md
      - name: Apply
        if: github.event_name == 'push'
        run: {{CMD}} policy apply --dir {{DIR}} --wait 600 --auto-approve
`

const gitlabPipeline = `# Include this file and set the variable {{ENV}}_URL to the gateway's address.
# Jobs sign in with GitLab's OIDC token: no secret is stored.
variables:
  {{ENV}}_POLICY_DIR: {{DIR}}

.{{CMD}}-policy:
  image: golang:latest
  id_tokens:
    {{ENV}}_ID_TOKEN:
      aud: ${{ENV}}_URL
  before_script:
    - go install github.com/patrikmichi/{{CMD}}/cmd/{{CMD}}@latest
    - export PATH="$PATH:$(go env GOPATH)/bin"

{{CMD}}-policy-plan:
  extends: .{{CMD}}-policy
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      changes: ["{{DIR}}/**/*"]
  script:
    - {{CMD}} policy plan --dir "${{ENV}}_POLICY_DIR" --format markdown --out plan.md
    - cat plan.md

{{CMD}}-policy-apply:
  extends: .{{CMD}}-policy
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
      changes: ["{{DIR}}/**/*"]
  script:
    - {{CMD}} policy apply --dir "${{ENV}}_POLICY_DIR" --wait 600 --auto-approve
`

// CI returns the file name and contents of a ready-made pipeline for provider
// (github or gitlab) that plans on review and applies on merge. dir is the
// policy directory.
func CI(provider, dir string) (name, content string, err error) {
	r := strings.NewReplacer("{{DIR}}", dir, "{{CMD}}", brand.Name, "{{ENV}}", strings.ToUpper(brand.Name))
	switch provider {
	case "github":
		return ".github/workflows/" + brand.Name + "-policy.yml", r.Replace(githubWorkflow), nil
	case "gitlab":
		return ".gitlab/" + brand.Name + "-policy.yml", r.Replace(gitlabPipeline), nil
	}
	return "", "", fmt.Errorf("unknown CI provider %q: use github or gitlab", provider)
}

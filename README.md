# terraform-deployer

Repository and Github Action for updating and releasing in Terraform Cloud

## Usage

If you have a Terraform Workflow with the variable `my_docker_image`, and you would like to update the value on each commit to `main` in your repository, you can create a `.github/workflows/release.yml` that looks something like this:

```yaml
name: Deploy
on:
  push:
    branches:
      - main
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Build and push
        id: push
        run: |
          docker build -t my-repo/my-app:latest .
          DOCKER_IMAGE="my-repo/my-app@$(docker push -q my-repo/my-app:latest)"
          echo "docker_image_value=${DOCKER_IMAGE}" >> $GITHUB_OUTPUT

      - name: Deploy to dev
        uses: xmtplabs/terraform-deployer@v1
        with:
          terraform-token: ${{ secrets.TERRAFORM_TOKEN }}
          terraform-org: ${{ secrets.TERRAFORM_ORG }}
          terraform-workspace: dev
          variable-name: my_docker_image
          variable-value: ${{ steps.push.outputs.docker_image_value }}
          variable-value-required-prefix: my-repo/my-app@
```

Every variable is written first, and then a *single* run is created for all of
them. If no variable's value actually changed, no run is created at all — an
auto-applied run applies the whole workspace, so a redeploy of something that is
already live should not drag unrelated pending changes out with it.

## Updating part of an HCL variable

Some variables are not a single image but a map of them. A fleet roster, for
instance, where each member pins its own image alongside configuration Terraform
owns:

```hcl
# herald_roster (HCL-typed workspace variable)
{
  a = {
    image       = "ghcr.io/xmtplabs/herald-lite:sha-7036d9b"
    archil_disk = "herald-dev/herald-a"
  }
  b = { ... }
}
```

Replacing the whole value would mean CI reconstructing the entire roster,
including the parts it has no business knowing. `variable-path` instead
addresses one string inside the variable and leaves everything else alone:

```yaml
- uses: xmtplabs/terraform-deployer@v1
  with:
    terraform-workspace: prod
    variable-name: herald_roster
    variable-path: a.image
    variable-value: ghcr.io/xmtplabs/herald-lite@sha256:...
    variable-value-required-prefix: "ghcr.io/xmtplabs/herald-lite@"
```

A single `variable-name` and a single `variable-value` broadcast across as many
paths as you give, so rolling several members at once is still one value:

```yaml
    variable-path: b.image,c.image,d.image
    variable-value: ghcr.io/xmtplabs/herald-lite@sha256:...
```

and a `*` segment fans out across every key at that level, so the fleet can be
rolled without CI knowing how many members it has:

```yaml
    variable-path: "*.image"
```

All of those land in one read-modify-write and one run, however many paths they
touch. Because `*` is idempotent for members already on the target image, the
two compose into a staggered rollout — canary first, then everything:

```yaml
jobs:
  canary:
    # variable-path: a.image
  rest:
    needs: canary
    # variable-path: "*.image"      # a is already there, so it is not a change
```

Each wave is its own run, and the run does not finish until Terraform has
finished applying it — so if the resources being rolled wait on their own health
(an ECS service with `wait_for_steady_state`, say), the next wave will not start
until the previous one is actually healthy.

### Rules

- **Replace-only, never upsert.** A variable that does not exist, a path segment
  that does not exist, or a `*` that matches no keys is an error. A deploy that
  silently creates something — or silently rolls nothing — is worse than one
  that fails.
- **The leaf must already be a string.** This is what stops a truncated path
  (`a` instead of `a.image`) from flattening a whole object into a bare image
  string.
- **The variable must be HCL-typed**, and it cannot be sensitive: Terraform Cloud
  never returns the value of a sensitive variable, so there is nothing to merge
  into. Whole-value replacement of a sensitive variable still works, since it
  never needs to read.
- The value is re-rendered as formatted HCL, so a variable a human maintains in
  the Terraform Cloud UI stays readable. Comments in the variable are not
  preserved.

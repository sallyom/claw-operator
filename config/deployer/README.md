# OpenClaw Deployer

This is a small OpenShift webapp for provisioning `Claw` resources in the
logged-in user's own OpenClaw namespace.

OpenShift OAuth protects the UI and forwards the username/groups to the
backend. The backend uses the deployer service account token with Kubernetes
impersonation headers, so OpenShift authorizes each request as the logged-in
user. The deployer can only operate where that user already has normal project
RBAC.

For each Claw, the deployer creates or deletes, only in that user's namespace:

- a provider API key Secret named `openclaw-<name>-<provider>-api-key`
- a `claw.sandbox.redhat.com/v1alpha1` `Claw`

## Build

```sh
make deployer-build DEPLOYER_IMG=quay.io/sallyom/openclaw-operator-deployer:deployer-ui
make deployer-push DEPLOYER_IMG=quay.io/sallyom/openclaw-operator-deployer:deployer-ui
```

## Deploy

```sh
oc new-project openclaw-deployer
oc create secret generic openclaw-deployer-cookie \
  --from-literal=session_secret="$(openssl rand -base64 32 | head -c 32)"

oc apply -k config/deployer
oc rollout status deployment/openclaw-deployer -n openclaw-deployer
oc get route openclaw-deployer -n openclaw-deployer
```

The manifests grant the deployer service account `impersonate` on users and
groups. They do not grant direct permission to create Secrets or Claws. Users
still need normal RBAC in the target namespace. If they can create Secrets and
`Claw` resources there, the card can provision their OpenClaw. If they cannot,
the app shows the Kubernetes authorization error.

By default, the deployer maps user `sallyom` to namespace `sallyom-claw`.
Users whose login already ends with `-claw` map to that exact namespace. Set
`CLAW_NAMESPACE_SUFFIX` on the deployer container to use a different suffix.

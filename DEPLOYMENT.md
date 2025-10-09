# Deployment Guide

This guide walks you through deploying the Cilium DHCP WAN IP Operator to your Kubernetes cluster.

## Prerequisites

1. **Kubernetes cluster** with Cilium installed and BGP configured
2. **Router access** via SSH (e.g., UDM-Pro, pfSense, etc.)
3. **Router script** installed at `/usr/local/bin/alloc_public_ip.sh` (see `config/samples/router-script-example.sh`)
4. **SSH key pair** for router authentication

## Step 1: Install CRDs

```bash
make install
```

This installs the PublicIPClaim CustomResourceDefinition to your cluster.

## Step 2: Set Up Router Script

Copy the router script to your router:

```bash
# Copy script to router
scp config/samples/router-script-example.sh root@192.168.1.1:/usr/local/bin/alloc_public_ip.sh

# Make it executable
ssh root@192.168.1.1 "chmod +x /usr/local/bin/alloc_public_ip.sh"
```

## Step 3: Create SSH Secret

Create a Kubernetes secret with your SSH private key:

```bash
kubectl -n kube-system create secret generic router-ssh \
  --from-file=id_rsa=/path/to/your/private/key
```

**Important**: The key must be named `id_rsa` in the secret.

## Step 4: Create Cilium IP Pool

Create a dedicated pool for the operator to manage:

```bash
kubectl apply -f config/samples/cilium-ippool-example.yaml
```

Edit the `serviceSelector` to match your services as needed.

## Step 5: Deploy the Operator

### Option A: Deploy to cluster

Build and push your image:

```bash
export IMG=your-registry/cilium-dhcp-wanip-operator:latest
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

### Option B: Run locally (for development)

```bash
make run
```

This runs the operator locally using your current kubeconfig.

## Step 6: Create a PublicIPClaim

Create your first IP claim:

```bash
# Edit config/samples/network_v1alpha1_publicipclaim.yaml
# Update host, user, wanParent fields

kubectl apply -f config/samples/network_v1alpha1_publicipclaim.yaml
```

## Step 7: Verify

Check the claim status:

```bash
kubectl get publicipclaims
kubectl get pics  # shortname
```

Expected output:
```
NAME         POOL          IP               PHASE   AGE
ip-wan-001   public-pool   203.0.113.45     Ready   1m
```

Check the Cilium pool:

```bash
kubectl get ciliumloadbalancerippools public-pool -o yaml
```

You should see your IP added to `spec.blocks`.

## Troubleshooting

### Check operator logs

```bash
kubectl -n cilium-dhcp-wanip-operator-system logs deployment/cilium-dhcp-wanip-operator-controller-manager
```

### Check claim status

```bash
kubectl describe publicipclaim ip-wan-001
```

### Common issues

1. **SSH connection fails**: Verify SSH key and router connectivity
2. **DHCP fails**: Check router WAN interface name (`wanParent`)
3. **IP not in pool**: Check Cilium pool permissions in RBAC

## Cleanup

To delete a claim (this removes the WAN interface from the router):

```bash
kubectl delete publicipclaim ip-wan-001
```

To uninstall the operator:

```bash
make undeploy  # if deployed to cluster
make uninstall # remove CRDs
```

## Next Steps

- Configure Cilium BGP to advertise routes to your router
- Create Services with LoadBalancer type
- Monitor operator metrics (when implemented)
- Set up multiple claims for additional IPs

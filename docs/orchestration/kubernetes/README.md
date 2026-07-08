# Deploy Buckit on Kubernetes

Buckit is a high performance distributed object storage server, designed for large-scale private cloud infrastructure. Buckit is designed in a cloud-native manner to scale sustainably in multi-tenant environments. Orchestration platforms like Kubernetes provide perfect cloud-native environment to deploy and scale Buckit.

## Buckit Deployment on Kubernetes

Use the Buckit Operator to create and update highly available distributed
Buckit clusters on Kubernetes. Refer to the
[Buckit Operator documentation](https://github.com/buckit-io/buckit-operator/blob/master/README.md)
for details.

The legacy in-repository Helm chart has been removed.

## Monitoring Buckit in Kubernetes

Buckit server exposes un-authenticated liveness endpoints so Kubernetes can natively identify unhealthy Buckit containers. Buckit also exposes Prometheus compatible data on a different endpoint to enable Prometheus users to natively monitor their Buckit deployments.

## Explore Further

- [Buckit Erasure Code QuickStart Guide](https://buckit-io.github.io/docs/community/minio-object-store/operations/concepts/erasure-coding.html)
- [Kubernetes Documentation](https://kubernetes.io/docs/home/)

# AWS services that use EBS volumes (gp2/gp3 applies)

Quick reference for which Terraform resources expose `gp2`/`gp3` attributes,
useful when planning a gp2 → gp3 migration sweep.

## Direct EBS — the volume_type attribute applies

| Service | Terraform resource | Attribute(s) | Notes |
|---|---|---|---|
| EC2 EBS volumes | `aws_ebs_volume` | `type` | Standalone volumes |
| EC2 instances | `aws_instance` | `root_block_device.volume_type`, `ebs_block_device.volume_type` | Most common in real configs |
| Launch templates | `aws_launch_template` | `block_device_mappings.ebs.volume_type` | Feeds ASGs and Spot Fleet |
| Launch configs (legacy) | `aws_launch_configuration` | `root_block_device.volume_type`, `ebs_block_device.volume_type` | Older ASG style |
| Spot Fleet | `aws_spot_fleet_request` | `launch_specification.root_block_device.volume_type`, `ebs_block_device.volume_type` | |
| RDS | `aws_db_instance` | `storage_type` | gp3 supported since Nov 2022 |
| Aurora cluster instances | `aws_rds_cluster_instance` | `storage_type` | Only for `db-` engines |
| EMR | `aws_emr_cluster` | `master_instance_group.ebs_config.type`, `core_instance_group.ebs_config.type`, `task_instance_group.ebs_config.type` | gp3 supported |
| EMR instance groups | `aws_emr_instance_group` | `ebs_config.type` | |
| EMR instance fleets | `aws_emr_instance_fleet` | `instance_type_configs.ebs_config.type` | |
| OpenSearch / Elasticsearch | `aws_opensearch_domain`, `aws_elasticsearch_domain` | `ebs_options.volume_type` | gp3 supported |
| MSK (Managed Kafka) | `aws_msk_cluster` | `broker_node_group_info.storage_info.ebs_storage_info.volume_type` | gp3 supported since 2023 |

## Use EBS underneath but don't expose volume_type

These ride on launch templates / EC2 — fixing the launch template covers them:

- EKS managed node groups (`aws_eks_node_group`)
- ECS EC2 capacity providers
- Auto Scaling Groups
- SageMaker (volume_size_in_gb only, no type)
- Glue dev endpoints

## Don't use EBS at all (skip)

- DocumentDB (`aws_docdb_cluster_instance`) — Aurora-style managed storage
- Neptune (`aws_neptune_cluster_instance`) — managed storage
- Aurora MySQL/Postgres — cluster-managed, not EBS
- WorkSpaces — managed, no exposed type attribute

## Heads-up before bulk-rewriting

- **MSK gp3** requires broker version checks and surrounding `provisioned_throughput`
  config may need updating.
- **OpenSearch** changing `volume_type` triggers a blue/green deployment. Not
  destructive, but takes time on big domains.
- **RDS** gp3 hits "provisioned IOPS-style" performance only above a minimum
  storage threshold (400 GB MySQL/Postgres/MariaDB, 200 GB SQL Server, 20 GB
  Oracle). Below that, gp3 baseline is 3000 IOPS / 125 MB/s. Usually fine,
  surprising for DBs sized just below the threshold.
- **EMR** transient clusters: change only matters for new clusters.

data "usaatags_resource_tags" "tags" {
  module = "my_lambda"
}

locals {
  tags            = data.usaatags_resource_tags.tags.tags
  lambda_role_arn = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/delegated/${data.usaatags_resource_tags.tags.application_id}/fis-lambda-role"
}

data "aws_caller_identity" "current" {}

### Create the CMK key for Lambda environment variable encryption
module "kms_key" {
  source  = "repo.usaa.com/usaa-terraform__usaa/terraform-aws-kms-key/aws"
  version = "9.2.0"

  app_name               = "fis-lambda"
  description            = "CMK for FIS Lambda function environment variable encryption"
  integrated_aws_services = ["lambda"]
  additional_role_arns = {
    (local.lambda_role_arn)                                                                            = "ED"
    "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/AWS_ITOPS_PCE_Dev-DevAdmin" = "ED"
  }
  tags = local.tags
}

data "aws_iam_policy_document" "lambda_policy" {
  # Grant the Lambda role encrypt/decrypt access to the KMS key
  statement {
    sid    = "AllowKMSEncryptDecrypt"
    effect = "Allow"
    actions = [
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:GenerateDataKey*",
      "kms:DescribeKey",
      "kms:CreateGrant"
    ]
    resources = [module.kms_key.key_arn]
  }

  # ... (add your other policy statements here)
}

# Fetch the FIS Lambda Extension layer ARN from SSM Parameter Store
data "aws_ssm_parameter" "fis_layer" {
  name = "/aws/service/fis/lambda-extension/AWS-FIS-extension-x86_64/1.x.x"
}

module "my_lambda" {
  source          = "repo.usaa.com/usaa-terraform__usaa/terraform-aws-lambda-function/aws"
  version         = "13.0.0"
  lambda_name     = "fis-lambda"
  cmk_key_arn     = module.kms_key.key_arn
  handler         = "index.handler"
  runtime         = "nodejs24.x"
  filename        = "${path.module}/fis-lambda.zip"
  tags            = local.tags
  iam_policy_document = data.aws_iam_policy_document.lambda_policy.json
  lambda_layers       = [data.aws_ssm_parameter.fis_layer.value]
  lambda_environment_variables = {
    variables = {
      "AWS_FIS_CONFIGURATION_LOCATION" = "arn:aws:s3:::fis-lambda-config-030215424959/FisConfigs/",
      "AWS_LAMBDA_EXEC_WRAPPER"        = "/opt/aws-fis/bootstrap"
    }
  }
  vpc_config = {
    subnet_ids         = data.aws_subnets.subnets.ids
    security_group_ids = [data.aws_security_group.main.id]
  }
}

### Get the single security group in order to be able to grab the vpc_id
data "aws_security_group" "main" {
  vpc_id = "vpc-0565031cca5236580"
  filter {
    name   = "group-name"
    values = ["usaa_equinix_sg"]
  }
}

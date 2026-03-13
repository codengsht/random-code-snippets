data "aws_iam_policy_document" "deny_fis_multi_account" {
  statement {
    sid    = "DenyFISMultiAccountExperiments"
    effect = "Deny"
    actions = [
      # Template-time: manage which accounts participate
      "fis:CreateTargetAccountConfiguration",
      "fis:UpdateTargetAccountConfiguration",
      "fis:DeleteTargetAccountConfiguration",
      "fis:GetTargetAccountConfiguration",
      "fis:ListTargetAccountConfigurations",
      # Experiment-time: view account config on running/completed experiments
      "fis:GetExperimentTargetAccountConfiguration",
      "fis:ListExperimentTargetAccountConfigurations"
    ]
    resources = ["*"]
  }
}

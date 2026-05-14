// Drop this validation block inside the existing `variable "volumes"` definition
// in the EC2 module (after the `default` attribute). It rejects any volume whose
// `type` is gp2 and surfaces a clear deprecation message.

validation {
  condition = alltrue([
    for v in var.volumes : lower(lookup(v, "type", "")) != "gp2"
  ])
  error_message = "gp2 is not recommended and is being deprecated. Use gp3 instead."
}

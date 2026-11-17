locals {
  name           = rig.release
  order_subjects = ["shop.orders.created", "shop.orders.paid", "shop.orders.cancelled"]
}

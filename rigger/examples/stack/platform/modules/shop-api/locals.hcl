locals {
  name           = rigger.release
  order_subjects = ["shop.orders.created", "shop.orders.paid", "shop.orders.cancelled"]
}

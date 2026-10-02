package catalog

// OrderableProduct 是下单所需的商品读模型：一件商品及其全部规格报价。
//
// 为什么不复用 Product：领域里的 Product 是面向检索与展示的模型，只有单一
// Price，既没有规格也没有库存。而确认单的每一行必须钉在「某个规格的价格」上——
// 同一件商品的不同规格价格不同，用商品级价格下单就是按错误的价格成交。
//
// 为什么定义在 domain：实现这个读模型的是基础设施层（Postgres 适配器），
// 若类型跟着端口留在 application/trade，infra 就必须反向依赖 application。
type OrderableProduct struct {
	ProductID string
	Title     string
	SKUs      []OrderableSKU
}

// OrderableSKU 是商品下一个规格的权威报价与库存。
//
// Price 用 Money 而不是最小单位整数：币种小数位、精度与溢出规则由 Money
// 统一承担，换算成最小单位只在交给账本前发生一次。
//
// Stock 为零表示「目录不知道自己有多少货」，而不是「没有货」——目录不承担
// 库存的权威性，真正的库存由交易账本的 trade_sku_inventory 决定，下单时的
// 扣减与校验都在那里发生。把目录的 0 当成「无货」会凭空拒掉合法订单。
type OrderableSKU struct {
	SKUID string
	Spec  string
	Price Money
	Stock int64
}

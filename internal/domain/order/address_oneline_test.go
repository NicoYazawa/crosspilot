package order

import (
	"strings"
	"testing"
)

// TestAddressOneLine 守住地址的一行展示格式。
//
// 一线字符串会被写进订单快照，是买家在订单详情里看到的收货信息。
// 它有两个容易写错的地方，因此逐条钉住：
//   - 空字段必须被跳过而不是留下连续空格——「CN  上海」这种空洞
//     会让展示层分不清是数据缺失还是排版问题；
//   - 收件人与电话要成对出现，缺一个就只剩一个，不能产出「（）」。
func TestAddressOneLine(t *testing.T) {
	cases := []struct {
		name string
		in   Address
		want string
	}{
		{
			name: "完整地址",
			in: Address{
				Recipient: "测试买家", Phone: "13800000000", Country: "CN",
				Province: "上海", City: "上海", Line1: "测试路 1 号",
				Line2: "2 单元 301", PostalCode: "200000",
			},
			want: "CN 上海 上海 测试路 1 号 2 单元 301 200000（测试买家 13800000000）",
		},
		{
			name: "只有必填字段",
			in: Address{
				Recipient: "测试买家", Phone: "13800000000", Country: "JP",
				City: "东京", Line1: "1-2-3",
			},
			want: "JP 东京 1-2-3（测试买家 13800000000）",
		},
		{
			name: "可选字段为空时不留下连续空格",
			in: Address{
				Recipient: "测试买家", Phone: "13800000000", Country: "CN",
				Province: "", City: "上海", Line1: "测试路 1 号",
				Line2: "", PostalCode: "",
			},
			want: "CN 上海 测试路 1 号（测试买家 13800000000）",
		},
		{
			name: "缺电话时括号里只有收件人",
			in: Address{
				Recipient: "测试买家", Country: "CN", City: "上海", Line1: "测试路 1 号",
			},
			want: "CN 上海 测试路 1 号（测试买家）",
		},
		{
			name: "缺收件人时括号里只有电话",
			in: Address{
				Phone: "13800000000", Country: "CN", City: "上海", Line1: "测试路 1 号",
			},
			want: "CN 上海 测试路 1 号（13800000000）",
		},
		{
			name: "收件人与电话都缺时完全不出现括号",
			in: Address{
				Country: "CN", City: "上海", Line1: "测试路 1 号",
			},
			want: "CN 上海 测试路 1 号",
		},
		{
			name: "字段带首尾空白时先裁掉再拼接",
			in: Address{
				Recipient: " 测试买家 ", Phone: " 13800000000 ", Country: " CN ",
				Province: " 上海 ", City: " 上海 ", Line1: " 测试路 1 号 ", PostalCode: " 200000 ",
			},
			want: "CN 上海 上海 测试路 1 号 200000（测试买家 13800000000）",
		},
		{
			name: "全空地址得到空串而不是一堆空格",
			in:   Address{},
			want: "",
		},
		{
			name: "只有空白字符等同于空",
			in: Address{
				Recipient: "  ", Phone: "\t", Country: " ", City: "\n", Line1: " ",
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.OneLine(); got != tc.want {
				t.Errorf("OneLine() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestAddressOneLineHasNoDoubledSpaces 守住「空字段被跳过」这条不变量。
//
// 逐个用例比对期望值已经能发现多余空格，但这条断言把意图写得更直白：
// 无论怎么组合空字段，结果里都不该出现连续空格或空的括号对。
func TestAddressOneLineHasNoDoubledSpaces(t *testing.T) {
	fields := []string{"", "X"}

	for _, country := range fields {
		for _, province := range fields {
			for _, city := range fields {
				for _, line1 := range fields {
					a := Address{
						Recipient: "买家", Phone: "13800000000",
						Country: country, Province: province, City: city, Line1: line1,
					}

					got := a.OneLine()
					if strings.Contains(got, "  ") {
						t.Errorf("地址 %+v 的一行结果出现连续空格：%q", a, got)
					}
					if strings.Contains(got, "（）") {
						t.Errorf("地址 %+v 的一行结果出现空括号：%q", a, got)
					}
				}
			}
		}
	}
}

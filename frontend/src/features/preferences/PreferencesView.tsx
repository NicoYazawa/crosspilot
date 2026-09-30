// 偏好视图（P6 仅展示本地 fixture；P7 接 P1 API）。
//
// 显示前端能识别的偏好项目（购物风格、语言、收藏品类），值来自前端
// fixture，无后端读写。P7 接 P1 后改用 fetch + form 控件。

interface PreferenceFixture {
  key: string;
  label: string;
  value: string;
  editable: boolean;
}

const fixtures: PreferenceFixture[] = [
  { key: 'lang',     label: '语言',     value: 'zh-CN', editable: false },
  { key: 'currency', label: '结算货币', value: 'CNY',   editable: false },
  { key: 'style',    label: '购物风格', value: '实用',  editable: false },
  { key: 'fav_cat',  label: '关注品类', value: '户外 / 数码 / 家居', editable: false },
];

export function PreferencesView() {
  return (
    <section>
      <h1>偏好</h1>
      <p className="muted">P6 仅展示本地 fixture；P7 接 P1 API 后可编辑</p>
      <table className="prefs-table">
        <thead>
          <tr>
            <th>项</th>
            <th>当前值</th>
          </tr>
        </thead>
        <tbody>
          {fixtures.map((f) => (
            <tr key={f.key}>
              <td>{f.label}</td>
              <td>{f.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

import { Component, type ReactNode } from 'react';

interface Props { children: ReactNode; }
interface State { error: Error | null; }

export class ErrorBoundary extends Component<Props, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error): void {
    // 上报到 /observability/.../errors 是后端的事情；前端先把错误打到 console
    console.error('[ErrorBoundary]', error);
  }

  override render(): ReactNode {
    if (this.state.error) {
      return (
        <div role="alert" style={{ padding: 24, color: '#c00' }}>
          <h2>渲染异常（A2UI 拒渲染或代码 bug）</h2>
          <pre style={{ whiteSpace: 'pre-wrap' }}>
            {this.state.error.message}
          </pre>
        </div>
      );
    }
    return this.props.children;
  }
}

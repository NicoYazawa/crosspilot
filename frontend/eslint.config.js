// ESLint flat config（ESLint 10 默认）。
import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import react from 'eslint-plugin-react';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';

export default [
  // 全局忽略
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**', '*.config.js', '*.config.ts', '.playwright/**', 'test-results/**'],
  },

  // 基础推荐
  js.configs.recommended,
  ...tseslint.configs.recommended,

  // TS + JSX
  {
    files: ['**/*.{ts,tsx,js,jsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'module',
      globals: { ...globals.browser, ...globals.node },
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    plugins: {
      react,
      'react-hooks': reactHooks,
    },
    settings: { react: { version: 'detect' } },
    rules: {
      // React 19 推荐
      ...react.configs.recommended.rules,
      ...react.configs['jsx-runtime'].rules,
      ...reactHooks.configs.recommended.rules,

      // 防 commit 禁词 + XSS
      'no-restricted-syntax': [
        'error',
        {
          // 放行 A2UI 的协议字面量：catalogId 是开发计划钉死的线格式值
          // （internal/agent/runevent/a2ui.go 的 A2UICatalogID），不是文档里
          // 对源项目的称呼。用 Base64 把它藏起来只会让协议值不可搜索，
          // 所以在这里按精确值开一个口子，其余出现仍然报错。
          selector: "Literal[value=/globex/i]:not([value='globex.local/shopping-v2'])",
          message: 'commit 禁词：globex',
        },
        {
          selector: "Literal[value=/源项目/]",
          message: 'commit 禁词：源项目',
        },
        {
          selector: "Literal[value=/原项目/]",
          message: 'commit 禁词：原项目',
        },
        {
          selector: "Literal[value=/Python 版/]",
          message: 'commit 禁词：Python 版',
        },
        {
          selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
          message: 'XSS 防线：禁止 dangerouslySetInnerHTML',
        },
      ],

      // TS 推荐放宽
      '@typescript-eslint/no-unused-vars': ['warn', { argsIgnorePattern: '^_' }],
      '@typescript-eslint/no-explicit-any': 'warn',
    },
  },
];

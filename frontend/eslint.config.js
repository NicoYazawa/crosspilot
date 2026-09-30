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
          selector: "Literal[value=/globex/i]",
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

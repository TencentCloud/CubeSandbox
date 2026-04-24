import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

export default withMermaid(defineConfig({
  title: "Cube Sandbox",
  description: "Production-grade, multi-component security sandbox system for serverless computing.",
  
  themeConfig: {
    socialLinks: [
      { icon: 'github', link: 'https://github.com/tencentcloud/CubeSandbox' }
    ]
  },

  locales: {
    root: {
      label: 'English',
      lang: 'en',
      themeConfig: {
        nav: [
          { text: 'Home', link: '/' },
          { text: 'Guide', link: '/guide/introduction' },
          { text: 'Architecture', link: '/architecture/overview' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/guide/': [
            {
              text: 'Getting Started',
              items: [
                { text: 'Introduction', link: '/guide/introduction' },
                { text: 'Quick Start', link: '/guide/quickstart' },
                { text: 'Self-Build Deployment', link: '/guide/self-build-deploy' },
                { text: 'Multi-Node Cluster', link: '/guide/multi-node-deploy' },
                { text: 'Development Environment (QEMU VM)', link: '/guide/dev-environment' }
              ]
            },
            {
              text: 'Core Concepts',
              items: [
                { text: 'Templates Overview', link: '/guide/templates' }
              ]
            },
            {
              text: 'Tutorials',
              items: [
                { text: 'Create Templates from OCI Image', link: '/guide/tutorials/template-from-image' },
                { text: 'Examples', link: '/guide/tutorials/examples' },
                { text: 'Custom Image', link: '/guide/tutorials/bring-your-own-image' }
              ]
            },
            {
              text: 'Operations',
              items: [
                { text: 'Template Inspection & Request Preview', link: '/guide/template-inspection-and-preview' },
                { text: 'HTTPS & Domain Resolution', link: '/guide/https-and-domain' },
                { text: 'Authentication', link: '/guide/authentication' }
              ]
            },
            {
              text: 'Developer Docs',
              items: [
                { text: 'Connect to an Existing Cube Cluster', link: '/guide/connect-existing-cluster' }
              ]
            }
          ],
          '/architecture/': [
            {
              text: 'System Design',
              items: [
                { text: 'Architecture Overview', link: '/architecture/overview' },
                { text: 'Networking (CubeVS)', link: '/architecture/network' }
              ]
            }
          ]
        }
      }
    },
    zh: {
      label: '简体中文',
      lang: 'zh',
      link: '/zh/',
      title: 'Cube Sandbox',
      description: '专为 Serverless 计算设计的生产级多组件安全沙箱系统。',
      themeConfig: {
        nav: [
          { text: '首页', link: '/zh/' },
          { text: '指南', link: '/zh/guide/introduction' },
          { text: '架构', link: '/zh/architecture/overview' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/zh/guide/': [
            {
              text: '入门指南',
              items: [
                { text: '简介 (Intro)', link: '/zh/guide/introduction' },
                { text: '快速开始', link: '/zh/guide/quickstart' },
                { text: '本地构建部署', link: '/zh/guide/self-build-deploy' },
                { text: '多机集群部署', link: '/zh/guide/multi-node-deploy' },
                { text: '开发环境（QEMU 虚机）', link: '/zh/guide/dev-environment' }
              ]
            },
            {
              text: '核心概念',
              items: [
                { text: '模板概览', link: '/zh/guide/templates' }
              ]
            },
            {
              text: '场景教程',
              items: [
                { text: '从 OCI 镜像制作模板', link: '/zh/guide/tutorials/template-from-image' },
                { text: '示例项目', link: '/zh/guide/tutorials/examples' },
                { text: '自定义镜像', link: '/zh/guide/tutorials/bring-your-own-image' }
              ]
            },
            {
              text: '安全与运维',
              items: [
                { text: '模板检查与请求预览', link: '/zh/guide/template-inspection-and-preview' },
                { text: 'HTTPS 证书与域名解析', link: '/zh/guide/https-and-domain' },
                { text: '鉴权', link: '/zh/guide/authentication' }
              ]
            },
            {
              text: '开发文档',
              items: [
                { text: '连接到已有 Cube 集群', link: '/zh/guide/connect-existing-cluster' }
              ]
            }
          ],
          '/zh/architecture/': [
            {
              text: '系统设计',
              items: [
                { text: '架构概览 (Overview)', link: '/zh/architecture/overview' },
                { text: 'CubeVS 网络模型', link: '/zh/architecture/network' }
              ]
            }
          ]
        }
      }
    }
  }
}))

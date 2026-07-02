import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

export default withMermaid(defineConfig({
  title: "Cube Sandbox",
  description: "Instant, Concurrent, Secure & Lightweight Sandbox Service for AI Agents",
  srcExclude: ['**/_template.md'],
  
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }]
  ],
  
  themeConfig: {
    outline: { level: [1, 3] },
    logo: '/logo.svg',
    socialLinks: [
      { icon: 'github', link: 'https://github.com/tencentcloud/CubeSandbox' }
    ],
    search: {
      provider: 'local',
      options: {
        miniSearch: {
          options: {
            tokenize: (text) => text
              ? text.split(/([\u4e00-\u9fa5])|[\s\W]+/).filter(Boolean)
              : []
          },
          searchOptions: {
            fuzzy: 0.2,
            prefix: true,
            boost: { title: 4, text: 2, titles: 1 }
          }
        },
        _render(src, env, md) {
          const html = md.render(src, env)
          if (env.frontmatter?.search === false) return ''
          const fm = env.frontmatter ?? {}
          if (!html.trim()) {
            // No markdown body (e.g. external-link blog posts).
            // Build a synthetic VitePress-style heading so splitPageIntoSections
            // picks it up. The actual page will redirect to externalUrl on load.
            if (!fm.title) return ''
            const slug = fm.title.toLowerCase()
              .replace(/\s+/g, '-')
              .replace(/[^\w-]/g, '')
              .replace(/-+/g, '-')
            return `<h1 id="${slug}" tabindex="-1">${fm.title} <a class="header-anchor" href="#${slug}">\u200B</a></h1>${fm.description ? `<p>${fm.description}</p>` : ''}`
          }
          // Inject frontmatter description right after the first heading so it
          // becomes part of that section's indexed text.
          if (fm.description) {
            return html.replace(/(<\/h[1-6]>)/, `$1<p>${fm.description}</p>`)
          }
          return html
        },
        locales: {
          root: {
            translations: {
              button: { buttonText: 'Search', buttonAriaLabel: 'Search docs' },
              modal: {
                displayDetails: 'Display detailed list',
                resetButtonTitle: 'Reset search',
                backButtonTitle: 'Close search',
                noResultsText: 'No results for',
                footer: {
                  selectText: 'to select',
                  navigateText: 'to navigate',
                  closeText: 'to close'
                }
              }
            }
          },
          zh: {
            translations: {
              button: { buttonText: '搜索', buttonAriaLabel: '搜索文档' },
              modal: {
                displayDetails: '显示详细列表',
                resetButtonTitle: '清空搜索',
                backButtonTitle: '关闭搜索',
                noResultsText: '没有找到相关结果',
                footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' }
              }
            }
          }
        }
      }
    }
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
          { text: 'Developer', link: '/dev/' },
          { text: 'Blog', link: '/blog/' },
          { text: 'About us', link: '/about-us' },
          { text: 'Changelog', link: '/changelog/' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/blog/': [],
          '/guide/': [
            {
              text: 'Getting Started',
              items: [
                { text: 'Introduction', link: '/guide/introduction' },
                { text: 'Quick Start', link: '/guide/quickstart' },
                { text: 'PVM Deployment', link: '/guide/pvm-deploy' },
                { text: 'Bare-Metal Deployment', link: '/guide/bare-metal-deploy' },
                { text: 'Multi-Node Cluster', link: '/guide/multi-node-deploy' },
                { text: 'Tencent Cloud Cluster (Terraform)', link: '/guide/tencentcloud-terraform-deploy' },
                { text: 'Self-Build Deployment', link: '/guide/self-build-deploy' },
                { text: 'Development Environment (QEMU VM)', link: '/guide/dev-environment' }
              ]
            },
            {
              text: 'Core Concepts',
              items: [
                { text: 'Sandbox Lifecycle', link: '/guide/lifecycle' },
                { text: 'Templates Overview', link: '/guide/templates' },
                { text: 'Snapshot, Rollback & Clone', link: '/guide/snapshot-rollback-clone' },
                { text: 'Digital Assistant', link: '/guide/digital-assistant' },
                { text: 'Performance Benchmark', link: '/guide/performance-benchmark' }
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
              text: 'Networking',
              items: [
                { text: 'Network Policy', link: '/guide/network-policy' },
                { text: 'Security Proxy', link: '/guide/security-proxy' },
                { text: 'Restrict Public Access', link: '/guide/restrict-public-access' }
              ]
            },
            {
              text: 'Operations',
              items: [
                { text: 'WebUI Dashboard', link: '/guide/webui' },
                { text: 'Service Management & Logs', link: '/guide/service-management' },
                { text: 'Sandbox Logs', link: '/guide/sandbox-logs' },
                { text: 'Template Inspection & Request Preview', link: '/guide/template-inspection-and-preview' },
                { text: 'HTTPS & Domain Resolution', link: '/guide/https-and-domain' },
                { text: 'Network Hardening', link: '/guide/network-hardening' },
                { text: 'Authentication', link: '/guide/authentication' }
              ]
            },
            {
              text: 'Integration',
              items: [
                { text: 'Connect to an Existing Cube Cluster', link: '/guide/connect-existing-cluster' }
              ]
            },
            {
              text: 'Contribute',
              items: [
                {
                  text: 'Troubleshooting',
                  link: '/guide/troubleshooting/',
                  items: [
                    { text: 'Deployment', link: '/guide/troubleshooting/deployment' },
                    { text: 'Templates', link: '/guide/troubleshooting/templates' },
                    { text: 'Host Mount Permissions', link: '/guide/troubleshooting/host-mount-permissions' }
                  ]
                },
                {
                  text: 'Use Cases',
                  link: '/guide/usecases/',
                  items: [
                    { text: 'trpc-agent-go', link: '/guide/usecases/trpc-agent-go' }
                  ]
                },
                { text: 'Integrations', link: '/guide/integrations/' }
              ]
            },
            {
              text: 'Maintainer Docs',
              items: [
                { text: 'Blog Maintenance', link: '/guide/maintainer/blog' }
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
          ],
          '/dev/': [
            {
              text: 'Developer Docs',
              items: [
                { text: 'Overview', link: '/dev/' },
                { text: 'Redis Key Convention', link: '/dev/redis-key-spec' }
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
      description: '一个极速启动、高并发、安全且轻量化的 AI Agent 沙箱服务',
      themeConfig: {
        nav: [
          { text: '首页', link: '/zh/' },
          { text: '指南', link: '/zh/guide/introduction' },
          { text: '架构', link: '/zh/architecture/overview' },
          { text: '开发者', link: '/zh/dev/' },
          { text: '博客', link: '/zh/blog/' },
          { text: '关于我们', link: '/zh/about-us' },
          { text: '更新日志', link: '/zh/changelog/' },
          { text: 'GitHub', link: 'https://github.com/tencentcloud/CubeSandbox' }
        ],
        sidebar: {
          '/zh/blog/': [],
          '/zh/guide/': [
            {
              text: '入门指南',
              items: [
                { text: '简介 (Intro)', link: '/zh/guide/introduction' },
                { text: '快速开始', link: '/zh/guide/quickstart' },
                { text: 'PVM部署', link: '/zh/guide/pvm-deploy' },
                { text: '裸金属/物理机部署', link: '/zh/guide/bare-metal-deploy' },
                { text: '多机集群部署', link: '/zh/guide/multi-node-deploy' },
                { text: '腾讯云集群部署（Terraform）', link: '/zh/guide/tencentcloud-terraform-deploy' },
                { text: '本地构建部署', link: '/zh/guide/self-build-deploy' },
                { text: '开发环境（QEMU 虚机）', link: '/zh/guide/dev-environment' }
              ]
            },
            {
              text: '核心概念',
              items: [
                { text: '沙箱生命周期', link: '/zh/guide/lifecycle' },
                { text: '模板概览', link: '/zh/guide/templates' },
                { text: '快照、回滚与克隆', link: '/zh/guide/snapshot-rollback-clone' },
                { text: '数字助手', link: '/zh/guide/digital-assistant' },
                { text: '性能测试', link: '/zh/guide/performance-benchmark' }
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
              text: '网络',
              items: [
                { text: '网络策略', link: '/zh/guide/network-policy' },
                { text: '安全代理', link: '/zh/guide/security-proxy' },
                { text: '限制公开访问', link: '/zh/guide/restrict-public-access' }
              ]
            },
            {
              text: '安全与运维',
              items: [
                { text: 'WebUI 控制台', link: '/zh/guide/webui' },
                { text: '服务管理与日志', link: '/zh/guide/service-management' },
                { text: '沙箱日志', link: '/zh/guide/sandbox-logs' },
                { text: '模板检查与请求预览', link: '/zh/guide/template-inspection-and-preview' },
                { text: 'HTTPS 证书与域名解析', link: '/zh/guide/https-and-domain' },
                { text: '网络加固', link: '/zh/guide/network-hardening' },
                { text: '鉴权', link: '/zh/guide/authentication' }
              ]
            },
            {
              text: '集成',
              items: [
                { text: '连接到已有 Cube 集群', link: '/zh/guide/connect-existing-cluster' }
              ]
            },
            {
              text: '社区共建',
              items: [
                {
                  text: '故障排障',
                  link: '/zh/guide/troubleshooting/',
                  items: [
                    { text: '部署相关', link: '/zh/guide/troubleshooting/deployment' },
                    { text: '模板相关', link: '/zh/guide/troubleshooting/templates' },
                    { text: 'Host Mount 权限', link: '/zh/guide/troubleshooting/host-mount-permissions' }
                  ]
                },
                {
                  text: '应用案例',
                  link: '/zh/guide/usecases/',
                  items: [
                    { text: 'trpc-agent-go', link: '/zh/guide/usecases/trpc-agent-go' }
                  ]
                },
                { text: '生态集成', link: '/zh/guide/integrations/' }
              ]
            },
            {
              text: '维护者文档',
              items: [
                { text: '博客维护', link: '/zh/guide/maintainer/blog' }
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
          ],
          '/zh/dev/': [
            {
              text: '开发者文档',
              items: [
                { text: '概览', link: '/zh/dev/' },
                { text: 'Redis Key 命名规范', link: '/zh/dev/redis-key-spec' }
              ]
            }
          ]
        }
      }
    }
  }
}))

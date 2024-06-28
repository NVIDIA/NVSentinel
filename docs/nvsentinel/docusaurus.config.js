// @ts-check
// `@type` JSDoc annotations allow editor autocompletion and type checking
// (when paired with `@ts-check`).
// There are various equivalent ways to declare your Docusaurus config.
// See: https://docusaurus.io/docs/api/docusaurus-config

import {themes as prismThemes} from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'NVSentinel',
  tagline: 'Cloud native fault detection',
  favicon: 'img/favicon.ico',

  // Set the production url of your site here
  url: 'https://dgxcloud.gitlab-master-pages.nvidia.com/',
  // Set the /<baseUrl>/ pathname under which your site is served
  // For GitHub pages deployment, it is often '/<projectName>/'
  baseUrl: '/mk8s/k8s-addons/nvsentinel/',

  // GitHub pages deployment config.
  // If you aren't using GitHub pages, you don't need these.
  organizationName: 'NVIDIA', // Usually your GitHub org/user name.
  projectName: 'nvsentinel', // Usually your repo name.

  onBrokenLinks: 'throw',
  onBrokenMarkdownLinks: 'warn',

  // Even if you don't use internationalization, you can use this field to set
  // useful metadata like html lang. For example, if your site is Chinese, you
  // may want to replace "en" with "zh-Hans".
  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          sidebarPath: './sidebars.js',
        },
        blog: {
          showReadingTime: true,
        },
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      // Replace with your project's social card
      image: 'img/docusaurus-social-card.jpg',
      navbar: {
        title: 'NVSentinel',
        logo: {
          alt: 'NVSentinel Logo',
          src: 'img/logo.jpg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'tutorialSidebar',
            position: 'left',
            label: 'Docs',
          },
          {to: '/blog', label: 'Top 5 Things', position: 'left'},
          {
            href: 'https://gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel',
            label: 'GitLab',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              {
                label: 'Tutorial',
                to: '/docs/intro',
              },
            ],
          },
          {
            title: 'Community',
            items: [
              {
                label: 'Slack',
                href: 'https://nvidia.enterprise.slack.com/archives/C070P7W91U6',
              },
            ],
          },
          {
            title: 'More',
            items: [
              {
                label: 'Top 5 Things',
                to: '/blog',
              },
              {
                label: 'GitLab',
                href: 'https://gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel',
              },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} NVIDIA`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
      },
    }),
};

export default config;

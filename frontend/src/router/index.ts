import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { public: true, title: '登录' },
    },
    {
      path: '/register',
      name: 'register',
      component: () => import('@/views/RegisterView.vue'),
      meta: { public: true, title: '注册' },
    },
    {
      // 被邀请人通常还没有账号，接受页必须公开；令牌本身是凭据。
      path: '/invite/:token',
      name: 'invite',
      component: () => import('@/views/AcceptInviteView.vue'),
      meta: { public: true, title: '接受邀请' },
    },
    {
      path: '/',
      component: () => import('@/layouts/AppLayout.vue'),
      children: [
        { path: '', redirect: '/expenses' },
        {
          path: 'expenses',
          name: 'expenses',
          component: () => import('@/views/ExpensesView.vue'),
          meta: { title: '费用单' },
        },
        {
          path: 'expenses/:id',
          name: 'expense-detail',
          component: () => import('@/views/ExpenseDetailView.vue'),
          meta: { title: '费用详情' },
        },
        {
          path: 'budgets',
          name: 'budgets',
          component: () => import('@/views/BudgetsView.vue'),
          meta: { title: '预算' },
        },
        {
          // 待办与审批策略都只对管理岗开放；服务端同样会拒绝普通成员。
          path: 'approvals',
          name: 'approvals',
          component: () => import('@/views/ApprovalsView.vue'),
          meta: { title: '审批', adminOnly: true },
        },
        {
          path: 'reports',
          name: 'reports',
          component: () => import('@/views/ReportsView.vue'),
          meta: { title: '报表' },
        },
        {
          path: 'billing',
          name: 'billing',
          component: () => import('@/views/BillingView.vue'),
          meta: { title: '账单' },
        },
        {
          path: 'notifications',
          name: 'notifications',
          component: () => import('@/views/NotificationsView.vue'),
          meta: { title: '通知' },
        },
        {
          path: 'members',
          name: 'members',
          component: () => import('@/views/MembersView.vue'),
          meta: { title: '成员', adminOnly: true },
        },
      ],
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'not-found',
      component: () => import('@/views/NotFoundView.vue'),
      meta: { public: true, title: '页面不存在' },
    },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.meta.public === true) {
    if (auth.isAuthenticated && (to.name === 'login' || to.name === 'register')) {
      return { path: '/expenses' }
    }
    return true
  }
  if (!auth.isAuthenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.meta.adminOnly === true && !auth.isAdmin) {
    return { path: '/expenses' }
  }
  return true
})

router.afterEach((to) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = title ? `${title} · Expensio` : 'Expensio'
})

export default router

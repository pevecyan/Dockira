import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Audit from './views/Audit.vue'
import Dashboard from './views/Dashboard.vue'
import Events from './views/Events.vue'
import Host from './views/Host.vue'
import Jobs from './views/Jobs.vue'
import Notifications from './views/Notifications.vue'
import Settings from './views/Settings.vue'
import Stack from './views/Stack.vue'
import Updates from './views/Updates.vue'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: Dashboard },
    { path: '/stacks/:id', component: Stack },
    { path: '/new', component: () => import('./views/NewApp.vue') },
    { path: '/updates', component: Updates },
    { path: '/jobs', component: Jobs },
    { path: '/events', component: Events },
    { path: '/audit', component: Audit },
    { path: '/host', component: Host },
    { path: '/notifications', component: Notifications },
    { path: '/settings', component: Settings },
    { path: '/:rest(.*)*', component: () => import('./views/NotFound.vue') },
  ],
})

createApp(App).use(router).mount('#app')

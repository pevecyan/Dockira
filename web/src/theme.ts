import { ref } from 'vue'

const KEY = 'dm.theme'
const media = window.matchMedia('(prefers-color-scheme: dark)')

function saved(): 'light' | 'dark' | null {
  try {
    const v = localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : null
  } catch {
    return null
  }
}

// index.html already applied the class before first paint; mirror it here.
export const dark = ref(document.documentElement.classList.contains('dark'))

function apply(d: boolean) {
  dark.value = d
  document.documentElement.classList.toggle('dark', d)
}

export function toggleTheme() {
  apply(!dark.value)
  try {
    localStorage.setItem(KEY, dark.value ? 'dark' : 'light')
  } catch {
    /* storage unavailable: the choice lasts for this page only */
  }
}

// Until the user picks a theme, follow the system setting live.
media.addEventListener('change', (e) => {
  if (!saved()) apply(e.matches)
})

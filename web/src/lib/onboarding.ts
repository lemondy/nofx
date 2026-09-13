export type UserMode = 'beginner' | 'advanced'

const USER_MODE_KEY = 'nofx_user_mode'

export function getUserMode(): UserMode | null {
  const value = localStorage.getItem(USER_MODE_KEY)
  if (value === 'beginner' || value === 'advanced') {
    return value
  }
  return null
}

export function setUserMode(mode: UserMode) {
  localStorage.setItem(USER_MODE_KEY, mode)
}

export function getPostAuthPath(_mode: UserMode | null | undefined): string {
  // Beginner claw402 onboarding was removed — both modes land on /traders.
  void _mode
  return '/traders'
}

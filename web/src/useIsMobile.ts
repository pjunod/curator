import { useEffect, useState } from 'react'

// The one breakpoint: below this the app wears its phone shell (bottom
// tabs + More sheet) and pages swap their cramped layouts (calendar grid
// → agenda). Keep in step with the @media rules in styles.css.
export const MOBILE_QUERY = '(max-width: 767px)'

export function useIsMobile(): boolean {
  const [mobile, setMobile] = useState(() =>
    typeof window !== 'undefined' ? window.matchMedia(MOBILE_QUERY).matches : false,
  )
  useEffect(() => {
    const mq = window.matchMedia(MOBILE_QUERY)
    const onChange = (e: MediaQueryListEvent) => setMobile(e.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  return mobile
}

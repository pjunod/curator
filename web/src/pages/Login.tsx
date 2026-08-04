import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { login } from '../api'

// LoginPage: session login when auth hardening is enabled (Phase 5).
export function LoginPage() {
  const navigate = useNavigate()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  const doLogin = useMutation({
    mutationFn: () => login(username, password),
    onSuccess: () => {
      void navigate({ to: '/' })
      // Queries made while logged out are stale-401; a reload is simplest.
      setTimeout(() => window.location.reload(), 50)
    },
  })

  return (
    <div style={{ maxWidth: 360, margin: '10vh auto' }}>
      <section className="panel">
        <h2>Sign in to Curator</h2>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            doLogin.mutate()
          }}
        >
          <div className="add-controls" style={{ flexDirection: 'column', alignItems: 'stretch', display: 'flex', gap: 8 }}>
            <input
              autoFocus
              placeholder="Username"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
            />
            <input
              type="password"
              placeholder="Password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <button className="btn-accent" type="submit" disabled={doLogin.isPending || !username}>
              Sign in
            </button>
          </div>
        </form>
        {doLogin.isError && (
          <div className="banner warning" style={{ marginTop: 10 }}>
            {String((doLogin.error as Error).message)}
          </div>
        )}
      </section>
    </div>
  )
}

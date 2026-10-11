import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App.tsx'
import { Toaster } from 'sonner'
import './index.css'
import { BrowserRouter } from 'react-router-dom'
import { initTheme, useTheme } from './lib/theme'

initTheme()

function ThemedToaster() {
  const theme = useTheme()
  return (
    <Toaster
      theme={theme}
      richColors
      closeButton
      position="top-center"
      duration={2200}
      toastOptions={{
        className: 'nofx-toast',
        style: {
          background: 'var(--surface)',
          border: '1px solid var(--line)',
          color: 'var(--fg)',
        },
      }}
    />
  )
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <ThemedToaster />
      <App />
    </BrowserRouter>
  </React.StrictMode>
)

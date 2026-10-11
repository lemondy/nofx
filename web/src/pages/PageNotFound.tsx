import { Home } from 'lucide-react'
import { Card, CardBody, buttonVariants } from '../components/ui'

export function PageNotFound() {
  return (
    <div className="flex min-h-[calc(100vh-64px)] items-center justify-center bg-bg p-4 text-center">
      <Card className="w-full max-w-md">
        <CardBody className="flex flex-col items-center gap-3 py-8">
          <h1 className="num text-5xl font-semibold text-fg">404</h1>
          <div className="text-sm font-semibold text-fg-2">Page not found</div>
          <p className="max-w-[40ch] text-[13px] leading-relaxed text-fg-3">
            The requested page does not exist. It may have been moved or
            deleted.
          </p>
          <a href="/" className={buttonVariants({ variant: 'primary' })}>
            <Home size={14} />
            <span>Back to home</span>
          </a>
        </CardBody>
      </Card>
    </div>
  )
}

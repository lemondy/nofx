import { useLanguage } from '../../contexts/LanguageContext'
import { Header } from '../common/Header'

export function ResetPasswordPage() {
  const { language } = useLanguage()
  return (
    <>
      <Header />
      <main className="max-w-lg mx-auto p-8">
        <h1 className="text-xl font-semibold">
          {language === 'zh' ? '账号恢复' : 'Account recovery'}
        </h1>
        <p className="mt-4">
          {language === 'zh'
            ? '未验证身份的密码重置已关闭。已登录用户可在设置中使用原密码修改密码；忘记密码请联系系统管理员，通过受控流程恢复账号。'
            : 'Unverified password reset is disabled. Signed-in users can change their password in Settings with their current password. Contact the system administrator for verified account recovery.'}
        </p>
        <a className="inline-block mt-4 underline" href="/login">
          {language === 'zh' ? '返回登录' : 'Back to login'}
        </a>
      </main>
    </>
  )
}

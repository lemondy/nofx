import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { DeepVoidBackground } from '../common/DeepVoidBackground'
import { Button, Card, CardBody } from '../ui'

export function ResetPasswordPage() {
  const { language } = useLanguage()
  return (
    <DeepVoidBackground disableAnimation>
      <div className="flex flex-1 items-center justify-center px-4 py-16">
        <div className="w-full max-w-[400px]">
          <div className="mb-6 text-center">
            <img
              src="/icons/nofx.svg"
              alt="NOFX"
              className="mx-auto mb-4 h-10 w-10"
            />
            <h1 className="text-xl font-semibold text-fg">
              {language === 'zh' ? '账号恢复' : 'Account recovery'}
            </h1>
          </div>
          <Card>
            <CardBody className="space-y-4">
              <p className="text-[13px] leading-relaxed text-fg-2">
                {language === 'zh'
                  ? '未验证身份的密码重置已关闭。已登录用户可在设置中使用原密码修改密码；忘记密码请联系系统管理员，通过受控流程恢复账号。'
                  : 'Unverified password reset is disabled. Signed-in users can change their password in Settings with their current password. Contact the system administrator for verified account recovery.'}
              </p>
              <Button
                variant="secondary"
                className="w-full"
                onClick={() => (window.location.href = '/login')}
              >
                {t('backToLogin', language)}
              </Button>
            </CardBody>
          </Card>
        </div>
      </div>
    </DeepVoidBackground>
  )
}

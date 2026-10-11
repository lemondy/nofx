import { useState, useMemo } from 'react'
import { HelpCircle } from 'lucide-react'
import { Button, buttonVariants } from '../ui'
import { t, type Language } from '../../i18n/translations'
import { FAQSearchBar } from './FAQSearchBar'
import { FAQSidebar } from './FAQSidebar'
import { FAQContent } from './FAQContent'
import { faqCategories } from '../../data/faqData'
import type { FAQCategory } from '../../data/faqData'

interface FAQLayoutProps {
  language: Language
}

export function FAQLayout({ language }: FAQLayoutProps) {
  const [searchTerm, setSearchTerm] = useState('')
  const [activeItemId, setActiveItemId] = useState<string | null>(null)

  // Filter categories based on search term
  const filteredCategories = useMemo(() => {
    if (!searchTerm.trim()) {
      return faqCategories
    }

    const term = searchTerm.toLowerCase()
    const filtered: FAQCategory[] = []

    faqCategories.forEach((category) => {
      const matchingItems = category.items.filter((item) => {
        const question = t(item.questionKey, language).toLowerCase()
        const answer = t(item.answerKey, language).toLowerCase()
        return question.includes(term) || answer.includes(term)
      })

      if (matchingItems.length > 0) {
        filtered.push({
          ...category,
          items: matchingItems,
        })
      }
    })

    return filtered
  }, [searchTerm, language])

  const handleItemClick = (_categoryId: string, itemId: string) => {
    const element = document.getElementById(itemId)
    if (element) {
      const offset = 100
      const elementPosition = element.getBoundingClientRect().top
      const offsetPosition = elementPosition + window.pageYOffset - offset

      window.scrollTo({
        top: offsetPosition,
        behavior: 'smooth',
      })
    }
  }

  return (
    <div className="min-h-screen bg-bg py-4 pt-20">
      <div className="mx-auto w-full max-w-[1280px] px-4 md:px-6">
        {/* Page Header */}
        <div className="mb-6 text-center">
          <div className="mb-3 flex items-center justify-center">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-brand-soft">
              <HelpCircle className="h-5 w-5 text-brand" />
            </div>
          </div>
          <h1 className="mb-1 text-xl font-semibold text-fg">
            {t('faqTitle', language)}
          </h1>
          <p className="mb-4 text-[13px] text-fg-3">
            {t('faqSubtitle', language)}
          </p>

          {/* Search Bar */}
          <div className="mx-auto max-w-xl">
            <FAQSearchBar
              searchTerm={searchTerm}
              onSearchChange={setSearchTerm}
              placeholder={
                language === 'zh' ? '搜索常见问题...' : 'Search FAQ...'
              }
            />
          </div>
        </div>

        {/* Main Content */}
        <div className="flex gap-4">
          {/* Sidebar - Hidden on mobile, visible on desktop */}
          <aside className="hidden w-64 flex-shrink-0 lg:block">
            <FAQSidebar
              categories={filteredCategories}
              activeItemId={activeItemId}
              language={language}
              onItemClick={handleItemClick}
            />
          </aside>

          {/* Content Area */}
          <main className="min-w-0 flex-1">
            {filteredCategories.length > 0 ? (
              <FAQContent
                categories={filteredCategories}
                language={language}
                onActiveItemChange={setActiveItemId}
              />
            ) : (
              <div className="py-12 text-center">
                <p className="text-sm text-fg-3">
                  {language === 'zh'
                    ? '没有找到匹配的问题'
                    : 'No matching questions found'}
                </p>
                <Button
                  variant="primary"
                  className="mt-3"
                  onClick={() => setSearchTerm('')}
                >
                  {language === 'zh' ? '清除搜索' : 'Clear Search'}
                </Button>
              </div>
            )}
          </main>
        </div>

        {/* Contact Section */}
        <div className="mt-8 rounded-lg border border-line bg-surface p-5 text-center">
          <h3 className="mb-1 text-sm font-semibold text-fg">
            {t('faqStillHaveQuestions', language)}
          </h3>
          <p className="mb-3 text-[13px] text-fg-3">
            {t('faqContactUs', language)}
          </p>
          <div className="flex items-center justify-center gap-2">
            <a
              href="https://github.com/NoFxAiOS/nofx"
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: 'secondary' })}
            >
              GitHub
            </a>
            <a
              href="https://t.me/nofx_dev_community"
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: 'primary' })}
            >
              {t('community', language)}
            </a>
          </div>
        </div>
      </div>
    </div>
  )
}

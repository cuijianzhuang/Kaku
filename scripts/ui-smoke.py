"""Real Go pages and form submissions; all fixtures are synthetic and local."""
import os,sys,shutil,re
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect
BASE=sys.argv[1].rstrip('/')
OUTPUT=os.environ.get('KAKU_UI_SCREENSHOTS')
if OUTPUT: Path(OUTPUT).mkdir(parents=True,exist_ok=True)

def screenshot(page,name):
    if OUTPUT: page.screenshot(path=str(Path(OUTPUT)/name),full_page=not name.endswith('-menu.png'))

def assert_layout(page):
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'page has horizontal overflow: '+page.url

def local_only(route):
    if urlparse(route.request.url).netloc == urlparse(BASE).netloc: route.continue_()
    else: route.abort()

with sync_playwright() as p:
    for engine in os.environ.get('KAKU_UI_BROWSERS','chromium,webkit').split(','):
        browser=p.chromium.launch(executable_path=os.environ.get('KAKU_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
        login=browser.new_context();login.route('**/*',local_only)
        page=login.new_page();page.goto(BASE+'/manage/login')
        page.locator('[name=username]').fill('admin');page.locator('[name=password]').fill('correct horse battery')
        page.get_by_role('button',name='登录',exact=True).click();page.wait_for_url(BASE+'/manage')
        state=login.storage_state();login.close()
        for width in [390,1440]:
            for theme in ['', 'paper','ocean','forest','sakura','midnight']:
                for mode in ['light','dark']:
                    context=browser.new_context(storage_state=state,viewport={'width':width,'height':844},is_mobile=width<760,has_touch=width<760)
                    context.route('**/*',local_only)
                    response=context.request.post(BASE+'/manage/settings',form={'section':'appearance','theme':theme,'mode':mode,'accent':''},headers={'Origin':BASE})
                    assert response.status==200
                    page=context.new_page();errors=[];page.on('pageerror',lambda error: errors.append(str(error)))
                    page.goto(BASE+'/');expect(page.locator('.product-card')).to_have_count(7);assert_layout(page)
                    expect(page.locator('meta[name=color-scheme]')).to_have_attribute('content','dark' if theme=='midnight' or mode=='dark' else 'light')
                    if theme=='':
                        search=page.get_by_role('searchbox',name='搜索商品或规格')
                        search.fill('套餐 2')
                        expect(page.locator('.product-card:visible')).to_have_count(1)
                        expect(page.locator('[data-filter-status]')).to_have_text('找到 1 件商品')
                        expect(page.locator('.chips a:visible')).to_have_count(1)
                        page.get_by_role('button',name='清除搜索',exact=True).click();expect(search).to_be_focused()
                        expect(page.locator('.product-card:visible')).to_have_count(7)
                        search.fill('不存在的商品')
                        expect(page.locator('[data-filter-empty]')).to_be_visible()
                        page.get_by_role('button',name='清除搜索，查看全部商品').click()
                        expect(search).to_be_focused();expect(page.locator('.chips a:visible')).to_have_count(2)
                    if theme=='paper' and width==390: screenshot(page,engine+'-'+mode+'-store.png')
                    page.goto(BASE+'/manage');assert_layout(page)
                    setup=page.locator('.setup-card');expect(setup.locator('summary')).to_contain_text('已完成')
                    assert not setup.evaluate('(e)=>e.open')
                    if theme=='':
                        setup.locator('summary').focus();page.keyboard.press('Enter');expect(setup.locator('.setup-list')).to_be_visible()
                        setup.locator('summary').click();expect(setup.locator('.setup-list')).not_to_be_visible()
                    if theme=='paper' and width==390: screenshot(page,engine+'-'+mode+'-dashboard.png')
                    if width<900:
                        opener=page.get_by_role('button',name='打开菜单',exact=True)
                        expect(page.locator('.sidebar')).not_to_be_visible()
                        opener.click();expect(opener).to_have_attribute('aria-expanded','true')
                        dialog=page.get_by_role('dialog',name='后台导航');expect(dialog).to_be_visible();expect(dialog).to_be_in_viewport(ratio=0.99)
                        if theme=='paper': screenshot(page,engine+'-'+mode+'-menu.png')
                        expect(dialog.get_by_role('button',name='关闭菜单')).to_be_focused()
                        assert page.evaluate('document.querySelector(".content").inert && getComputedStyle(document.body).overflow === "hidden"')
                        dialog.locator('.brand').focus();page.keyboard.press('Shift+Tab')
                        expect(dialog.get_by_role('button',name='退出登录')).to_be_focused()
                        page.keyboard.press('Tab');expect(dialog.locator('.brand')).to_be_focused()
                        page.keyboard.press('Escape');expect(opener).to_be_focused();expect(opener).to_have_attribute('aria-expanded','false')
                        opener.click();page.locator('.scrim').click(position={'x':width-8,'y':250})
                        expect(opener).to_have_attribute('aria-expanded','false')
                        opener.click();page.get_by_role('dialog',name='后台导航').get_by_role('link',name='订单',exact=True).click()
                        page.wait_for_url(BASE+'/manage/orders');expect(page.locator('.sidebar')).not_to_be_visible()
                        assert not page.locator('.content').evaluate('(e)=>e.inert')
                        if theme=='' and mode=='light':
                            page.go_back();expect(page.get_by_role('button',name='打开菜单')).to_have_attribute('aria-expanded','false')
                            page.get_by_role('button',name='打开菜单').click()
                            page.set_viewport_size({'width':1100,'height':844})
                            expect(page.locator('.sidebar')).to_be_visible();assert not page.locator('.content').evaluate('(e)=>e.inert')
                            page.set_viewport_size({'width':width,'height':844});expect(page.locator('.sidebar')).not_to_be_visible()
                        page.goto(BASE+'/manage/orders')
                    else:
                        page.locator('.sidebar').get_by_role('link',name='订单',exact=True).click()
                    expect(page.get_by_role('textbox',name='搜索订单')).to_be_visible()
                    expect(page.get_by_role('combobox',name='订单状态')).to_be_visible();assert_layout(page)
                    page.goto(BASE+'/manage/settings/appearance');expect(page.get_by_role('radio',name=re.compile('纸张'))).to_have_count(1);assert_layout(page)
                    page.goto(BASE+'/p/2');assert_layout(page)
                    if theme=='' and mode=='light':
                        page.get_by_label('邮箱（接收发货通知，用于查单）').fill('ui-fixture@example.com')
                        page.get_by_label('查询密码（查单时使用，至少 4 位）').fill('synthetic-query-password')
                        page.get_by_role('button',name='增加',exact=True).click()
                        expect(page.locator('[name=qty]')).to_have_value('2')
                        expect(page.locator('[data-total]')).to_contain_text('38')
                        page.get_by_role('button',name='立即购买',exact=True).click()
                        page.wait_for_url(re.compile(re.escape(BASE)+r'/pay/'))
                        expect(page.locator('.amount-tag .price')).to_contain_text('38');expect(page.locator('[data-poll]')).to_be_visible();assert_layout(page)
                    assert not errors, errors
                    context.close();print(f'PASS {engine} {width} {theme or "simple"} {mode}',flush=True)
        # Extra narrow widths and the navigation breakpoint.
        for width in [320,360,900,901]:
            context=browser.new_context(storage_state=state,viewport={'width':width,'height':844});context.route('**/*',local_only)
            page=context.new_page()
            for path in ['/','/p/2','/manage','/manage/orders','/manage/settings/appearance']:
                page.goto(BASE+path);assert_layout(page)
            context.close()
        # The native setup disclosure and original label navigation still work without JS.
        context=browser.new_context(storage_state=state,viewport={'width':390,'height':844},java_script_enabled=False);context.route('**/*',local_only)
        page=context.new_page();page.goto(BASE+'/manage')
        page.locator('.setup-summary').click();expect(page.locator('.setup-list')).to_be_visible()
        page.locator('label.mbar-btn[for=nav-toggle]').click();expect(page.locator('.sidebar')).to_be_visible()
        context.close();browser.close();print(f'PASS {engine} narrow breakpoints and no JavaScript',flush=True)

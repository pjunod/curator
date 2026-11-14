import {test,expect,request as apiRequest,type APIRequestContext} from '@playwright/test'
import {spawn,type ChildProcess} from 'node:child_process'
import {mkdtempSync,mkdirSync} from 'node:fs'
import {tmpdir} from 'node:os'
import {join,resolve} from 'node:path'
import {createServer} from 'node:net'

test.describe.configure({mode:'serial'})
test.setTimeout(60000)
let child:ChildProcess,base:string,api:APIRequestContext,rootID:number,logs=''
const password='household-regression-password'
const freePort=()=>new Promise<number>((done,reject)=>{const s=createServer();s.once('error',reject);s.listen(0,'127.0.0.1',()=>{const port=(s.address() as {port:number}).port;s.close(()=>done(port))})})
test.beforeEach(async()=>{
 const port=await freePort(),dir=mkdtempSync(join(tmpdir(),'curator-household-browser-')),root=join(dir,'movies');mkdirSync(root);base=`http://127.0.0.1:${port}`;logs=''
 child=spawn(resolve(import.meta.dirname,'../../../bin/monarr'),[],{env:{...process.env,MONARR_HOST:'127.0.0.1',MONARR_PORT:String(port),MONARR_DATA_DIR:dir,MONARR_LOG_LEVEL:'warn',MONARR_TMDB_BASE_URL:`http://127.0.0.1:${process.env.FAKE_TMDB_PORT??7788}`,MONARR_TVMAZE_BASE_URL:`http://127.0.0.1:${process.env.FAKE_TMDB_PORT??7788}`},stdio:['ignore','pipe','pipe']});child.stdout?.on('data',data=>{logs+=String(data)});child.stderr?.on('data',data=>{logs+=String(data)})
 api=await apiRequest.newContext({baseURL:base});await expect.poll(async()=>{try{return (await api.get('/api/v1/system/status')).status()}catch{return 0}},{timeout:15000}).toBe(200)
 expect((await api.put('/api/v1/settings',{data:{tmdbApiKey:'fixture-key',authUsername:'owner',authPassword:password,authRequired:true}})).status()).toBe(204)
 const owner=await(await api.post('/api/v1/auth/sessions',{data:{username:'owner',password,device:'test owner'}})).json();const headers={Authorization:`Bearer ${owner.token}`}
 const rootResponse=await api.post('/api/v1/rootfolders',{headers,data:{path:root,kind:'mixed'}});expect(rootResponse.ok()).toBeTruthy();rootID=(await rootResponse.json()).id
 const invitation=await(await api.post('/api/v1/household/invitations',{headers,data:{password}})).json();expect((await api.post('/api/v1/auth/invitations/accept',{data:{token:invitation.token,username:'member',displayName:'Household member',password}})).status()).toBe(201)
})
test.afterEach(async({},info)=>{if(info.status!==info.expectedStatus)await info.attach('household-server.log',{body:logs,contentType:'text/plain'});await api?.dispose();if(child&&child.exitCode===null){child.kill('SIGTERM');await new Promise<void>(done=>{const timer=setTimeout(()=>{child.kill('SIGKILL');done()},3000);child.once('exit',()=>{clearTimeout(timer);done()})})}})
for(const width of [1280,390])test(`personal household request and owner review at ${width}px`,async({page,browser},info)=>{
 await page.setViewportSize({width,height:900});const ownerContext=await browser.newContext({viewport:{width,height:900}});const ownerPage=await ownerContext.newPage()
 try{
  await page.goto(base+'/login');await page.getByPlaceholder('Username',{exact:true}).fill('member');await page.getByPlaceholder('Password',{exact:true}).fill(password);await page.getByRole('button',{name:'Sign in',exact:true}).click();await expect(page.getByRole('heading',{name:'Discover',exact:true})).toBeVisible()
  const forbidden:string[]=[];page.on('request',req=>{const path=new URL(req.url()).pathname;if(path.startsWith('/api/v1/')&&!path.startsWith('/api/v1/household/')&&!path.startsWith('/api/v1/auth/'))forbidden.push(path)})
  await page.goto(base+'/settings');await expect(page.getByRole('heading',{name:'Discover',exact:true})).toBeVisible();await expect(page.getByRole('link',{name:'Activity',exact:true})).toHaveCount(0)
  await page.getByLabel('Find a title',{exact:true}).fill('The Test Movie');await page.getByRole('button',{name:'Search',exact:true}).click();await page.getByRole('button',{name:/The Test Movie/}).first().click();const dialog=page.getByRole('dialog');await expect(dialog.getByRole('button',{name:'Send request',exact:true})).toBeEnabled();await dialog.getByRole('button',{name:'Send request',exact:true}).click();const receipt=dialog.getByRole('status').filter({hasText:/Following request/});await expect(receipt).toBeVisible();const bounds=await receipt.boundingBox();expect(bounds!.y).toBeGreaterThanOrEqual(0);expect(bounds!.y+bounds!.height).toBeLessThanOrEqual(901);await page.screenshot({path:info.outputPath(`household-member-${width}.png`)})
  await dialog.getByRole('link',{name:'View requests',exact:true}).click();await expect(page.getByRole('dialog')).toHaveCount(0);await expect(page.getByRole('heading',{name:'Requests',exact:true})).toBeVisible();expect(forbidden).toEqual([])
  await ownerPage.goto(base+'/login');await ownerPage.getByPlaceholder('Username',{exact:true}).fill('owner');await ownerPage.getByPlaceholder('Password',{exact:true}).fill(password);await ownerPage.getByRole('button',{name:'Sign in',exact:true}).click();await expect(ownerPage.getByRole('heading',{name:'Sign in to Curator',exact:true})).toHaveCount(0);await ownerPage.goto(base+'/requests');await ownerPage.getByRole('button',{name:/The Test Movie/}).first().click();const review=ownerPage.getByRole('dialog');await review.getByLabel('Destination for a new title').selectOption(String(rootID));const approve=review.getByRole('button',{name:'Approve reviewed missing scope',exact:true});await expect(approve).toBeEnabled();const box=await approve.boundingBox();if(box!.y<0||box!.y+box!.height>900)await approve.scrollIntoViewIfNeeded();await approve.click();const status=review.getByRole('status').filter({hasText:'Approved for the reviewed missing scope.'});await expect(status).toBeVisible();const statusBox=await status.boundingBox();expect(statusBox!.y).toBeGreaterThanOrEqual(0);expect(statusBox!.y+statusBox!.height).toBeLessThanOrEqual(901);await ownerPage.screenshot({path:info.outputPath(`household-owner-review-${width}.png`)})
  await page.reload();await expect(page.getByRole('button',{name:/The Test Movie/}).first()).toContainText(/Approved|Acquiring/);expect(forbidden).toEqual([])
 }finally{await ownerContext.close()}
})

test('an owner switch in another tab resets private request rows and open review state',async({page,context})=>{
 const owner=await(await api.post('/api/v1/auth/sessions',{data:{username:'owner',password,device:'owner setup'}})).json(),headers={Authorization:`Bearer ${owner.token}`}
 const accounts=await(await api.get('/api/v1/household/accounts',{headers})).json(),member=accounts.find((a:{username:string})=>a.username==='member')
 expect((await api.patch(`/api/v1/household/accounts/${member.id}`,{headers,data:{revision:member.revision,password,role:'owner',status:'active'}})).status()).toBe(204)
 const other=await(await api.post('/api/v1/auth/sessions',{data:{username:'member',password,device:'second owner'}})).json()
 const titles=await(await api.get('/api/v1/household/catalogue?kind=movie&q=The%20Test%20Movie',{headers})).json()
 const submitted=await(await api.post('/api/v1/household/requests',{headers,data:{kind:'movie',tmdbId:titles[0].tmdbId,seasons:[]}})).json()
 expect((await api.put(`/api/v1/household/requests/${submitted.id}/note`,{headers,data:{note:'Owner A private note'}})).status()).toBe(204)
 expect((await api.put(`/api/v1/household/requests/${submitted.id}/note`,{headers:{Authorization:`Bearer ${other.token}`},data:{note:'Owner B private note'}})).status()).toBe(204)
 const signIn=async(tab:typeof page,username:string)=>{await tab.goto(base+'/login');await tab.getByPlaceholder('Username',{exact:true}).fill(username);await tab.getByPlaceholder('Password',{exact:true}).fill(password);await tab.getByRole('button',{name:'Sign in',exact:true}).click();await expect(tab.getByRole('heading',{name:'Sign in to Curator',exact:true})).toHaveCount(0)}
 await signIn(page,'owner');await page.goto(base+'/requests');await page.getByRole('button',{name:/The Test Movie/}).first().click();await expect(page.getByLabel('Private note · only you can see this')).toHaveValue('Owner A private note')
 const second=await context.newPage();await signIn(second,'member');await page.bringToFront();await expect(page.getByRole('dialog')).toHaveCount(0,{timeout:20000});await page.getByRole('button',{name:/The Test Movie/}).first().click();await expect(page.getByLabel('Private note · only you can see this')).toHaveValue('Owner B private note');await expect(page.locator('textarea').filter({hasText:'Owner A private note'})).toHaveCount(0);await second.close()
})

test('a stale member form cannot submit for the account signed in by another tab',async({page,context})=>{
 const owner=await(await api.post('/api/v1/auth/sessions',{data:{username:'owner',password,device:'setup'}})).json(),headers={Authorization:`Bearer ${owner.token}`}
 const invite=await(await api.post('/api/v1/household/invitations',{headers,data:{password}})).json();expect((await api.post('/api/v1/auth/invitations/accept',{data:{token:invite.token,username:'another-member',displayName:'Another member',password}})).status()).toBe(201)
 await page.goto(base+'/login');await page.getByPlaceholder('Username',{exact:true}).fill('member');await page.getByPlaceholder('Password',{exact:true}).fill(password);await page.getByRole('button',{name:'Sign in',exact:true}).click();await expect(page.getByRole('heading',{name:'Discover',exact:true})).toBeVisible()
 const original=await(await page.request.get(base+'/api/v1/auth/me')).json();await page.route('**/api/v1/auth/me',route=>route.fulfill({json:original}))
 await page.getByLabel('Find a title',{exact:true}).fill('The Test Movie');await page.getByRole('button',{name:'Search',exact:true}).click();await page.getByRole('button',{name:/The Test Movie/}).first().click();await expect(page.getByRole('button',{name:'Send request',exact:true})).toBeEnabled()
 const second=await context.newPage();await second.goto(base+'/login');await second.getByPlaceholder('Username',{exact:true}).fill('another-member');await second.getByPlaceholder('Password',{exact:true}).fill(password);await second.getByRole('button',{name:'Sign in',exact:true}).click();await expect(second.getByRole('heading',{name:'Discover',exact:true})).toBeVisible()
 const refused=page.waitForResponse(r=>r.url().endsWith('/api/v1/household/requests')&&r.request().method()==='POST');await page.getByRole('button',{name:'Send request',exact:true}).click();expect((await refused).status()).toBe(409);await expect(page.getByRole('dialog')).toContainText('Account changed');const list=await(await page.request.get(base+'/api/v1/household/requests')).json();expect(list).toEqual([]);await second.close()
})

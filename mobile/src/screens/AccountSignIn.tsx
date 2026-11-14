import {CinemaSignIn} from './CinemaSignIn'
import {useEffect,useRef,useState,type MutableRefObject} from 'react'
import {Platform,Text,View} from 'react-native'
import {MonarrClient} from '../api'
import {normalizeServerUrl} from '../connection'
import type {Connection} from '../types'
import {useTheme} from '../theme'
import {Button,Field,InlineError,Panel} from '../components/UI'

export function AccountSignIn({server,onConnected,attempts:shared}:{server:string;onConnected:(value:Connection)=>Promise<void>;attempts?:MutableRefObject<number>}){
 const own=useRef(0),attempts=shared??own,selected=useRef(server),alive=useRef(true),locked=useRef(false);if(selected.current!==server){selected.current=server;if(!shared)attempts.current++}
 useEffect(()=>{alive.current=true;return()=>{alive.current=false;attempts.current++}},[])
 const theme=useTheme(),[username,setUsername]=useState(''),[password,setPassword]=useState(''),[error,setError]=useState(''),[busy,setBusy]=useState(false)
 const signIn=async()=>{
  if(locked.current)return;locked.current=true;setBusy(true);setError('');const epoch=++attempts.current
  const valid=()=>alive.current&&attempts.current===epoch
  let client:MonarrClient|undefined,token=''
  try{const baseUrl=normalizeServerUrl(server);client=new MonarrClient({baseUrl,apiKey:''});const result=await client.createAccountSession(username,password,`Curator ${Platform.OS}`);token=result.token
   if(!valid()){await new MonarrClient({baseUrl,apiKey:'',sessionToken:token}).logoutAccount();return}
   await onConnected({baseUrl,apiKey:'',sessionToken:token});token='';if(valid())setPassword('')
  }catch(cause){if(token&&client){try{await new MonarrClient({...client.connection,apiKey:'',sessionToken:token}).logoutAccount()}catch{if(alive.current)setError('Unused sign-in could not be revoked. It will expire.')}}if(valid())setError(cause instanceof Error?cause.message:'Sign-in failed.')}finally{locked.current=false;if(alive.current)setBusy(false)}
 }

 return <Panel><Text style={{color:theme.text,fontSize:18,fontWeight:'600'}}>Use your Curator account</Text><Text style={{color:theme.muted}}>Enter the server address above, then your own local username and password. Cinema is optional.</Text><View style={{gap:12}}><Field accessibilityLabel="Curator username" placeholder="Username" autoCapitalize="none" autoComplete="username" value={username} onChangeText={setUsername}/><Field accessibilityLabel="Curator password" placeholder="Password" autoCapitalize="none" secureTextEntry autoComplete="current-password" value={password} onChangeText={setPassword} onSubmitEditing={()=>void signIn()}/><Button label={busy?'Signing in…':'Sign in to Curator'} disabled={busy||!server.trim()||!username||!password} onPress={()=>void signIn()}/><InlineError message={error}/><CinemaSignIn attempts={attempts} server={server} onConnected={onConnected}/></View></Panel>
}

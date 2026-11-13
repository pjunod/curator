import {CinemaSignIn} from './CinemaSignIn'
import {useState} from 'react'
import {Platform,Text,View} from 'react-native'
import {MonarrClient} from '../api'
import {normalizeServerUrl} from '../connection'
import type {Connection} from '../types'
import {useTheme} from '../theme'
import {Button,Field,InlineError,Panel} from '../components/UI'

export function AccountSignIn({server,onConnected}:{server:string;onConnected:(value:Connection)=>Promise<void>}){
 const theme=useTheme(),[username,setUsername]=useState(''),[password,setPassword]=useState(''),[error,setError]=useState(''),[busy,setBusy]=useState(false)
 const signIn=async()=>{if(busy)return;setBusy(true);setError('');try{const baseUrl=normalizeServerUrl(server);const result=await new MonarrClient({baseUrl,apiKey:''}).createAccountSession(username,password,`Curator ${Platform.OS}`);await onConnected({baseUrl,apiKey:'',sessionToken:result.token});setPassword('')}catch(cause){setError(cause instanceof Error?cause.message:'Sign-in failed.')}finally{setBusy(false)}}
 return <Panel><Text style={{color:theme.text,fontSize:18,fontWeight:'600'}}>Use your Curator account</Text><Text style={{color:theme.muted}}>Enter the server address above, then your own local username and password. Cinema is optional.</Text><View style={{gap:12}}><Field accessibilityLabel="Curator username" placeholder="Username" autoCapitalize="none" autoComplete="username" value={username} onChangeText={setUsername}/><Field accessibilityLabel="Curator password" placeholder="Password" autoCapitalize="none" secureTextEntry autoComplete="current-password" value={password} onChangeText={setPassword} onSubmitEditing={()=>void signIn()}/><Button label={busy?'Signing in…':'Sign in to Curator'} disabled={busy||!server.trim()||!username||!password} onPress={()=>void signIn()}/><InlineError message={error}/><CinemaSignIn server={server} onConnected={onConnected}/></View></Panel>
}
